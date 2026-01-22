// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/fifo"
	"github.com/containerd/typeurl/v2"
	vctypes "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/types"
	"github.com/opencontainers/runtime-spec/specs-go"
)

type exec struct {
	id          string
	token       string
	containerID string
	spec        *specs.Process
	status      task.Status
	exitCode    int32
	exitTime    time.Time
	terminal    bool

	stdin  string
	stdout string
	stderr string

	ioMu        sync.Mutex
	ioWg        sync.WaitGroup
	ioAttached  bool
	stdinCloser io.Closer
	stdinFifo   io.Closer
	ioCancel    context.CancelFunc

	exitCh   chan struct{}
	exitOnce sync.Once
	exitIOch chan struct{}
}

func newExec(containerID, execID, stdin, stdout, stderr string, terminal bool, specAny typeurl.Any) (*exec, error) {
	if specAny == nil {
		return nil, fmt.Errorf("exec spec is required")
	}

	v, err := typeurl.UnmarshalAny(specAny)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal exec spec: %w", err)
	}

	var processSpec *specs.Process
	switch s := v.(type) {
	case *specs.Process:
		processSpec = s
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal exec spec for conversion: %w", err)
		}
		processSpec = &specs.Process{}
		if err := json.Unmarshal(data, processSpec); err != nil {
			return nil, fmt.Errorf("failed to convert exec spec to Process: %w", err)
		}
	}

	return &exec{
		id:          execID,
		containerID: containerID,
		spec:        processSpec,
		status:      task.Status_CREATED,
		terminal:    terminal,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		exitCh:      make(chan struct{}),
		exitIOch:    make(chan struct{}),
	}, nil
}

func (e *exec) closeExitCh() {
	e.exitOnce.Do(func() {
		close(e.exitCh)
	})
}

func (s *service) startExec(ctx context.Context, c *container, execID string) error {
	c.execsMu.RLock()
	e, ok := c.execs[execID]
	c.execsMu.RUnlock()

	if !ok {
		return fmt.Errorf("exec %s not found in container %s", execID, c.id)
	}

	sandbox := s.getSandbox()
	if sandbox == nil {
		return fmt.Errorf("sandbox not found")
	}

	s.log.WithField("container", c.id).WithField("exec", execID).Info("starting exec process")

	cmd := vctypes.Cmd{
		Args:         e.spec.Args,
		Envs:         envsToVCEnvs(e.spec.Env),
		WorkDir:      e.spec.Cwd,
		User:         fmt.Sprintf("%d", e.spec.User.UID),
		PrimaryGroup: fmt.Sprintf("%d", e.spec.User.GID),
		Interactive:  e.terminal,
		Detach:       !e.terminal,
	}

	if e.spec.Capabilities != nil {
		cmd.Capabilities = e.spec.Capabilities
	}

	for _, gid := range e.spec.User.AdditionalGids {
		cmd.SupplementaryGroups = append(cmd.SupplementaryGroups, fmt.Sprintf("%d", gid))
	}

	if e.spec.NoNewPrivileges {
		cmd.NoNewPrivileges = true
	}

	_, proc, err := sandbox.EnterContainer(ctx, c.id, cmd)
	if err != nil {
		return fmt.Errorf("failed to enter container: %w", err)
	}

	e.token = proc.Token
	s.log.WithField("exec", execID).WithField("token", e.token).Info("exec process created with token")

	e.status = task.Status_RUNNING

	if err := s.handleExecIO(s.ctx, c, e); err != nil {
		s.log.WithError(err).Warn("failed to handle exec IO")
	}

	go s.waitExecProcess(c, e)

	s.log.WithField("container", c.id).WithField("exec", execID).Info("exec process started")

	return nil
}

func (s *service) waitExecProcess(c *container, e *exec) {
	defer func() {
		if r := recover(); r != nil {
			s.log.WithField("panic", r).WithField("exec", e.id).Error("waitExecProcess panic recovered")
		}
	}()

	s.log.WithField("container", c.id).WithField("exec", e.id).WithField("token", e.token).Info("waitExecProcess started")

	sandbox := s.getSandbox()
	if sandbox == nil {
		s.log.WithField("exec", e.id).Error("sandbox is nil in waitExecProcess")
		e.status = task.Status_STOPPED
		e.exitCode = 255
		e.exitTime = time.Now()
		e.closeExitCh()
		return
	}

	exitCode, err := sandbox.WaitProcess(s.ctx, c.id, e.token)
	if err != nil {
		s.log.WithError(err).WithField("exec", e.id).WithField("token", e.token).Error("WaitProcess failed for exec")
		if exitCode == 0 {
			exitCode = 255
		}
	}

	s.log.WithField("exec", e.id).WithField("exit", exitCode).Info("exec process exited, waiting for IO")

	select {
	case <-e.exitIOch:
		s.log.WithField("exec", e.id).Info("exec IO streams closed")
	case <-time.After(defaultOperationTimeout):
		s.log.WithField("exec", e.id).Warn("timeout waiting for exec IO streams")
	}

	e.status = task.Status_STOPPED
	e.exitCode = int32(exitCode)
	e.exitTime = time.Now()

	e.closeExitCh()

	s.log.WithField("exec", e.id).WithField("exitCode", exitCode).Info("exec process wait completed")
}

func (s *service) handleExecIO(ctx context.Context, c *container, e *exec) error {
	e.ioMu.Lock()
	if e.ioAttached {
		e.ioMu.Unlock()
		s.log.WithField("exec", e.id).Debug("exec IO already attached, skipping")
		return nil
	}

	s.log.WithField("exec", e.id).
		WithField("stdin", e.stdin).
		WithField("stdout", e.stdout).
		WithField("stderr", e.stderr).
		Info("handleExecIO called")

	if e.stdin == "" && e.stdout == "" && e.stderr == "" {
		s.log.WithField("exec", e.id).Info("no exec IO paths provided, skipping IO setup")
		e.ioAttached = true
		e.ioMu.Unlock()
		close(e.exitIOch)
		return nil
	}

	sandbox := s.getSandbox()
	if sandbox == nil {
		e.ioMu.Unlock()
		return fmt.Errorf("sandbox not found")
	}

	stdinStream, stdoutStream, stderrStream, err := sandbox.IOStream(c.id, e.token)
	if err != nil {
		e.ioMu.Unlock()
		return fmt.Errorf("failed to get exec IO stream: %w", err)
	}

	e.ioAttached = true
	ioCtx, ioCancel := context.WithCancel(ctx)
	e.ioCancel = ioCancel
	e.ioMu.Unlock()

	s.log.WithField("exec", e.id).Info("attaching exec IO streams")

	var stdinFifo io.ReadCloser
	if e.stdin != "" && stdinStream != nil {
		e.stdinCloser = stdinStream
		f, err := fifo.OpenFifo(ioCtx, e.stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			s.log.WithError(err).WithField("path", e.stdin).Warn("failed to open exec stdin fifo")
		} else {
			stdinFifo = f
			e.stdinFifo = f
			e.ioWg.Add(1)
			go s.copyExecStdin(e, stdinStream, stdinFifo)
		}
	}

	if e.stdout != "" && stdoutStream != nil {
		e.ioWg.Add(1)
		go s.copyExecStdout(ioCtx, e, stdoutStream, stdinFifo)
	}

	if e.stderr != "" && stderrStream != nil {
		e.ioWg.Add(1)
		go s.copyExecStderr(ioCtx, e, stderrStream)
	}

	go func() {
		e.ioWg.Wait()
		s.log.WithField("exec", e.id).Debug("all exec IO streams closed")
		close(e.exitIOch)
	}()

	return nil
}

func (s *service) copyExecStdin(e *exec, dst io.WriteCloser, src io.ReadCloser) {
	defer e.ioWg.Done()
	if _, err := io.Copy(dst, src); err != nil && err != context.Canceled {
		s.log.WithError(err).WithField("exec", e.id).Debug("exec stdin copy ended")
	}
	s.log.WithField("exec", e.id).Debug("exec stdin copy goroutine exited")
	dst.Close()
}

func (s *service) copyExecStdout(ctx context.Context, e *exec, src io.Reader, stdinFifo io.Closer) {
	defer e.ioWg.Done()

	f, err := fifo.OpenFifo(ctx, e.stdout, syscall.O_RDWR, 0)
	if err != nil {
		s.log.WithError(err).WithField("path", e.stdout).Warn("failed to open exec stdout fifo")
		return
	}
	defer f.Close()

	s.log.WithField("exec", e.id).WithField("path", e.stdout).Info("exec stdout fifo opened, starting copy")
	n, err := io.Copy(f, src)
	s.log.WithField("exec", e.id).WithField("bytes", n).Info("exec stdout copy completed")
	if err != nil && err != context.Canceled {
		s.log.WithError(err).WithField("exec", e.id).Debug("exec stdout copy ended with error")
	}

	if stdinFifo != nil {
		s.log.WithField("exec", e.id).Debug("exec stdout done, closing stdin fifo")
		stdinFifo.Close()
	}
}

func (s *service) copyExecStderr(ctx context.Context, e *exec, src io.Reader) {
	defer e.ioWg.Done()

	f, err := fifo.OpenFifo(ctx, e.stderr, syscall.O_RDWR, 0)
	if err != nil {
		s.log.WithError(err).WithField("path", e.stderr).Warn("failed to open exec stderr fifo")
		return
	}
	defer f.Close()

	s.log.WithField("exec", e.id).WithField("path", e.stderr).Debug("exec stderr fifo opened")
	if _, err := io.Copy(f, src); err != nil && err != context.Canceled {
		s.log.WithError(err).WithField("exec", e.id).Debug("exec stderr copy ended")
	}
}

func (s *service) deleteExec(c *container, execID string) (*exec, error) {
	c.execsMu.Lock()
	e, ok := c.execs[execID]
	if !ok {
		c.execsMu.Unlock()
		return nil, fmt.Errorf("exec %s not found in container %s", execID, c.id)
	}

	e.ioMu.Lock()
	if e.ioCancel != nil {
		e.ioCancel()
	}
	e.ioMu.Unlock()

	delete(c.execs, execID)
	c.execsMu.Unlock()

	e.closeExitCh()

	s.log.WithField("container", c.id).WithField("exec", execID).Info("exec deleted")

	return e, nil
}

func envsToVCEnvs(envs []string) []vctypes.EnvVar {
	result := make([]vctypes.EnvVar, 0, len(envs))
	for _, env := range envs {
		for i := 0; i < len(env); i++ {
			if env[i] == '=' {
				result = append(result, vctypes.EnvVar{
					Var:   env[:i],
					Value: env[i+1:],
				})
				break
			}
		}
	}
	return result
}
