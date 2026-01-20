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

// Container lifecycle state machine:
//
//	CREATED -> RUNNING -> STOPPED
//	    |         |
//	    v         v
//	  Delete    Delete
//
// IO lifecycle:
//
//	1. handleIO() starts IO copy goroutines
//	2. waitContainerProcess() waits for process exit
//	3. Process exits -> stdout/stderr get EOF
//	4. IO goroutines finish -> exitIOch is closed
//	5. waitContainerProcess() updates container status

func (s *service) startContainer(ctx context.Context, c *container) error {
	sandbox := s.getSandbox()
	if sandbox == nil {
		return fmt.Errorf("sandbox not found for container %s", c.id)
	}

	s.log.WithField("container", c.id).WithField("type", c.cType).Info("starting container")

	if c.cType.IsSandbox() {
		// Start the sandbox - this will start the container process
		if err := sandbox.Start(ctx); err != nil {
			return fmt.Errorf("failed to start sandbox: %w", err)
		}

		// Use service context for long-running monitor
		monitor, err := sandbox.Monitor(s.ctx)
		if err != nil {
			s.log.WithError(err).Warn("failed to start sandbox monitor")
		} else {
			go s.watchSandbox(s.ctx, monitor)
		}

		// Attach IO synchronously - use service context for long-running IO
		if err := s.handleIO(s.ctx, c); err != nil {
			s.log.WithError(err).Warn("failed to attach IO")
		}

		if err := katautils.EnterNetNS(sandbox.GetNetNs(), func() error {
			return katautils.PostStartHooks(ctx, *c.spec, sandbox.ID(), c.bundle)
		}); err != nil {
			s.log.WithError(err).Warn("failed to run post-start hooks")
		}

	} else {
		// For non-sandbox containers, handleIO directly using service context
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

	// Start background waiter goroutine
	go s.waitContainerProcess(c)

	s.log.WithField("container", c.id).Info("container started successfully")

	return nil
}

// waitContainerProcess runs in background to wait for container process exit
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
		c.exitCh <- 255
		return
	}

	// 1. Wait for the process to exit first
	exitCode, err := sandbox.WaitProcess(s.ctx, c.id, c.id)
	if err != nil {
		s.log.WithError(err).WithField("container", c.id).Error("WaitProcess failed")
		if exitCode == 0 {
			exitCode = 255
		}
	}

	s.log.WithField("container", c.id).WithField("exit", exitCode).Info("container process exited, waiting for IO")

	// 2. Wait for IO streams to drain and close
	select {
	case <-c.exitIOch:
		s.log.WithField("container", c.id).Info("IO streams closed")
	case <-time.After(defaultOperationTimeout):
		s.log.WithField("container", c.id).Warn("timeout waiting for IO streams, continuing")
	}

	exitTime := time.Now()

	// 3. Update container status atomically
	s.mu.Lock()
	c.status = task.Status_STOPPED
	c.exit = uint32(exitCode)
	c.exitTime = exitTime
	s.mu.Unlock()

	// 4. Send exit code to channel for Wait() to receive
	c.exitCh <- uint32(exitCode)

	// 5. Handle sandbox cleanup for sandbox containers
	s.cleanupAfterExit(c)
}

// cleanupAfterExit handles post-exit cleanup for containers
func (s *service) cleanupAfterExit(c *container) {
	cleanupCtx, cancel := withCleanupTimeout()
	defer cancel()

	if c.cType.IsSandbox() {
		s.log.WithField("container", c.id).Debug("sandbox container exited, stopping sandbox")

		sandbox := s.getSandbox()
		if sandbox == nil {
			s.log.WithField("container", c.id).Warn("sandbox already nil during cleanup")
			return
		}

		if err := sandbox.Stop(cleanupCtx, true); err != nil {
			s.log.WithError(err).Warn("failed to stop sandbox")
		}

		if err := sandbox.Delete(cleanupCtx); err != nil {
			s.log.WithError(err).Warn("failed to delete sandbox")
		} else {
			s.clearSandbox()
			s.log.Debug("sandbox deleted and reference cleared")
		}
	} else {
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

// handleIO sets up IO streams between container and FIFOs
// IO lifecycle:
//  1. Open FIFOs for stdin/stdout/stderr
//  2. Start copy goroutines
//  3. When process exits, stdout/stderr streams get EOF
//  4. Stdout goroutine closes stdin FIFO to unblock stdin goroutine
//  5. All goroutines exit, ioWg.Wait() returns, exitIOch is closed
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

	// If no IO paths are provided, mark as attached and return
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

	// Get IO streams from sandbox
	stdinStream, stdoutStream, stderrStream, err := sandbox.IOStream(c.id, c.id)
	if err != nil {
		c.ioMu.Unlock()
		return fmt.Errorf("failed to get IO stream: %w", err)
	}

	c.ioAttached = true
	ioCtx, ioCancel := context.WithCancel(ctx)
	c.ioCancel = ioCancel
	c.ioMu.Unlock()

	s.log.WithField("container", c.id).Info("attaching IO streams")

	// Setup stdin
	var stdinFifo io.ReadCloser
	if c.stdin != "" && stdinStream != nil {
		c.stdinCloser = stdinStream
		f, err := fifo.OpenFifo(ioCtx, c.stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			s.log.WithError(err).WithField("path", c.stdin).Warn("failed to open stdin fifo")
		} else {
			stdinFifo = f
			c.stdinFifo = f
			c.ioWg.Add(1)
			go s.copyStdin(c, stdinStream, stdinFifo)
		}
	}

	// Setup stdout
	if c.stdout != "" && stdoutStream != nil {
		c.ioWg.Add(1)
		go s.copyStdout(ioCtx, c, stdoutStream, stdinFifo)
	}

	// Setup stderr
	if c.stderr != "" && stderrStream != nil {
		c.ioWg.Add(1)
		go s.copyStderr(ioCtx, c, stderrStream)
	}

	// Start goroutine to signal when all IO is done
	go func() {
		c.ioWg.Wait()
		s.log.WithField("container", c.id).Debug("all IO streams closed")
		close(c.exitIOch)
	}()

	return nil
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

	// Close stdin FIFO to unblock stdin goroutine
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
