// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/fifo"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
)

func (s *service) startContainer(ctx context.Context, c *container) error {
	sandbox := s.getSandbox()
	if sandbox == nil {
		return fmt.Errorf("sandbox not found for container %s", c.id)
	}

	s.log.WithField("container", c.id).WithField("type", c.cType).Info("starting container")

	if c.cType.IsSandbox() {
		if err := sandbox.Start(ctx); err != nil {
			return fmt.Errorf("failed to start sandbox: %w", err)
		}

		monitor, err := sandbox.Monitor(s.ctx)
		if err != nil {
			s.log.WithError(err).Warn("failed to start sandbox monitor")
		} else {
			go s.watchSandbox(s.ctx, monitor)
		}

		if err := s.handleIO(s.ctx, c); err != nil {
			s.log.WithError(err).Warn("failed to attach IO")
		}

		if err := katautils.EnterNetNS(sandbox.GetNetNs(), func() error {
			return katautils.PostStartHooks(ctx, *c.spec, sandbox.ID(), c.bundle)
		}); err != nil {
			s.log.WithError(err).Warn("failed to run post-start hooks")
		}

	} else {
		if err := s.handleIO(s.ctx, c); err != nil {
			s.log.WithError(err).Warn("failed to handle IO")
		}

		if _, err := sandbox.StartContainer(ctx, c.id); err != nil {
			return fmt.Errorf("failed to start container: %w", err)
		}

		if err := katautils.PostStartHooks(ctx, *c.spec, sandbox.ID(), c.bundle); err != nil {
			s.log.WithError(err).Warn("failed to run post-start hooks")
		}
	}

	c.status = task.Status_RUNNING

	go s.waitContainerProcess(c)

	s.log.WithField("container", c.id).Info("container started successfully")

	return nil
}

func (s *service) waitContainerProcess(c *container) {
	defer func() {
		if r := recover(); r != nil {
			s.log.WithField("panic", r).WithField("container", c.id).Error("waitContainerProcess panic recovered")
		}
	}()

	s.log.WithField("container", c.id).Info("waitContainerProcess started")

	sandbox := s.getSandbox()
	if sandbox == nil {
		s.log.WithField("container", c.id).Error("sandbox is nil in waitContainerProcess")
		s.mu.Lock()
		c.status = task.Status_STOPPED
		c.exit = 255
		c.exitTime = time.Now()
		s.mu.Unlock()
		c.closeExitCh()
		return
	}

	exitCode, err := sandbox.WaitProcess(s.ctx, c.id, c.id)
	if err != nil {
		s.log.WithError(err).WithField("container", c.id).Error("WaitProcess failed")
		if exitCode == 0 {
			exitCode = 255
		}
	}

	s.log.WithField("container", c.id).WithField("exit", exitCode).Info("container process exited, waiting for IO")

	select {
	case <-c.exitIOch:
		s.log.WithField("container", c.id).Info("IO streams closed")
	case <-time.After(defaultOperationTimeout):
		s.log.WithField("container", c.id).Warn("timeout waiting for IO streams, continuing")
	}

	exitTime := time.Now()

	s.mu.Lock()
	c.status = task.Status_STOPPED
	c.exit = uint32(exitCode)
	c.exitTime = exitTime
	s.mu.Unlock()

	c.closeExitCh()

	s.cleanupAfterExit(c)
}

func (s *service) cleanupAfterExit(c *container) {
	if c.cType.IsSandbox() {
		s.log.WithField("container", c.id).Debug("sandbox container exited, delegating to doSandboxCleanup")
		s.doSandboxCleanup()
	} else {
		cleanupCtx, cancel := withCleanupTimeout()
		defer cancel()
		sandbox := s.getSandbox()
		if sandbox != nil {
			if _, err := sandbox.StopContainer(cleanupCtx, c.id, true); err != nil {
				s.log.WithError(err).Warn("failed to stop container")
			}
		}
	}
}

func (s *service) watchSandbox(ctx context.Context, monitor chan error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.WithField("panic", r).Error("watchSandbox panic recovered")
		}
	}()

	select {
	case err := <-monitor:
		if err != nil {
			s.log.WithError(err).Error("sandbox monitor error")
		} else {
			s.log.Info("sandbox exited normally")
		}
	case <-ctx.Done():
		s.log.Info("sandbox monitor stopped")
	}
}

func (s *service) handleIO(ctx context.Context, c *container) error {
	c.ioMu.Lock()
	if c.ioAttached {
		c.ioMu.Unlock()
		s.log.WithField("container", c.id).Debug("IO already attached, skipping")
		return nil
	}

	s.log.WithField("container", c.id).
		WithField("stdin", c.stdin).
		WithField("stdout", c.stdout).
		WithField("stderr", c.stderr).
		Info("handleIO called")

	if c.stdin == "" && c.stdout == "" && c.stderr == "" {
		s.log.WithField("container", c.id).Info("no IO paths provided, skipping IO setup")
		c.ioAttached = true
		c.ioMu.Unlock()
		close(c.exitIOch)
		return nil
	}

	sandbox := s.getSandbox()
	if sandbox == nil {
		c.ioMu.Unlock()
		return fmt.Errorf("sandbox not found")
	}

	stdinStream, stdoutStream, stderrStream, err := sandbox.IOStream(c.id, c.id)
	if err != nil {
		c.ioMu.Unlock()
		return fmt.Errorf("failed to get IO stream: %w", err)
	}

	ioSetupSuccess := false
	defer func() {
		if !ioSetupSuccess {
			s.log.WithField("container", c.id).Warn("IO setup failed, cleaning up streams")
			s.closeIOStreams(stdinStream, stdoutStream, stderrStream)
		}
	}()

	c.ioAttached = true
	ioCtx, ioCancel := context.WithCancel(ctx)
	c.ioCancel = ioCancel
	c.ioMu.Unlock()

	s.log.WithField("container", c.id).Info("attaching IO streams")

	var stdinUsed, stdoutUsed, stderrUsed bool

	var stdinFifo io.ReadCloser
	if c.stdin != "" && stdinStream != nil {
		c.stdinCloser = stdinStream
		f, err := fifo.OpenFifo(ioCtx, c.stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			s.log.WithError(err).WithField("path", c.stdin).Warn("failed to open stdin fifo")
			if closer, ok := stdinStream.(io.Closer); ok {
				closer.Close()
			}
		} else {
			stdinFifo = f
			c.stdinFifo = f
			c.ioWg.Add(1)
			stdinUsed = true
			go s.copyStdin(c, stdinStream, stdinFifo)
		}
	}

	if c.stdout != "" && stdoutStream != nil {
		c.ioWg.Add(1)
		stdoutUsed = true
		go s.copyStdout(ioCtx, c, stdoutStream, stdinFifo)
	}

	if c.stderr != "" && stderrStream != nil {
		c.ioWg.Add(1)
		stderrUsed = true
		go s.copyStderr(ioCtx, c, stderrStream)
	}

	if !stdinUsed && stdinStream != nil {
		if closer, ok := stdinStream.(io.Closer); ok {
			closer.Close()
		}
	}
	if !stdoutUsed && stdoutStream != nil {
		if closer, ok := stdoutStream.(io.Closer); ok {
			closer.Close()
		}
	}
	if !stderrUsed && stderrStream != nil {
		if closer, ok := stderrStream.(io.Closer); ok {
			closer.Close()
		}
	}

	ioSetupSuccess = true

	go func() {
		c.ioWg.Wait()
		s.log.WithField("container", c.id).Debug("all IO streams closed")
		close(c.exitIOch)
	}()

	return nil
}

func (s *service) closeIOStreams(streams ...interface{}) {
	for _, stream := range streams {
		if stream == nil {
			continue
		}
		if closer, ok := stream.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				s.log.WithError(err).Debug("failed to close IO stream during cleanup")
			}
		}
	}
}

func (s *service) copyStdin(c *container, dst io.WriteCloser, src io.ReadCloser) {
	defer c.ioWg.Done()
	if _, err := io.Copy(dst, src); err != nil && err != context.Canceled {
		s.log.WithError(err).Debug("stdin copy ended")
	}
	s.log.WithField("container", c.id).Debug("stdin copy goroutine exited")
	dst.Close()
}

func (s *service) copyStdout(ctx context.Context, c *container, src io.Reader, stdinFifo io.Closer) {
	defer c.ioWg.Done()

	f, err := fifo.OpenFifo(ctx, c.stdout, syscall.O_RDWR, 0)
	if err != nil {
		s.log.WithError(err).WithField("path", c.stdout).Warn("failed to open stdout fifo")
		return
	}
	defer f.Close()

	s.log.WithField("path", c.stdout).Info("stdout fifo opened, starting copy")
	n, err := io.Copy(f, src)
	s.log.WithField("bytes", n).WithField("path", c.stdout).Info("stdout copy completed")
	if err != nil && err != context.Canceled {
		s.log.WithError(err).Debug("stdout copy ended with error")
	}

	if stdinFifo != nil {
		s.log.WithField("container", c.id).Debug("stdout done, closing stdin fifo")
		stdinFifo.Close()
	}
}

func (s *service) copyStderr(ctx context.Context, c *container, src io.Reader) {
	defer c.ioWg.Done()

	f, err := fifo.OpenFifo(ctx, c.stderr, syscall.O_RDWR, 0)
	if err != nil {
		s.log.WithError(err).WithField("path", c.stderr).Warn("failed to open stderr fifo")
		return
	}
	defer f.Close()

	s.log.WithField("path", c.stderr).Debug("stderr fifo opened")
	if _, err := io.Copy(f, src); err != nil && err != context.Canceled {
		s.log.WithError(err).Debug("stderr copy ended")
	}
}
