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
	if s.sandbox == nil {
		return fmt.Errorf("sandbox not found for container %s", c.id)
	}

	s.log.WithField("container", c.id).WithField("type", c.cType).Info("starting container")

	if c.cType.IsSandbox() {
		// Start the sandbox - this will start the container process
		if err := s.sandbox.Start(ctx); err != nil {
			return fmt.Errorf("failed to start sandbox: %w", err)
		}

		// Use background context for long-running monitor since request context will be cancelled
		monitorCtx := context.Background()
		monitor, err := s.sandbox.Monitor(monitorCtx)
		if err != nil {
			s.log.WithError(err).Warn("failed to start sandbox monitor")
		} else {
			go s.watchSandbox(monitorCtx, monitor)
		}

		// Attach IO synchronously - like kata-shim-v2
		// Use background context since s.ctx may be cancelled when the Start request completes
		if err := s.handleIO(context.Background(), c); err != nil {
			s.log.WithError(err).Warn("failed to attach IO")
		}

		if err := katautils.EnterNetNS(s.sandbox.GetNetNs(), func() error {
			return katautils.PostStartHooks(ctx, *c.spec, s.sandbox.ID(), c.bundle)
		}); err != nil {
			s.log.WithError(err).Warn("failed to run post-start hooks")
		}

	} else {
		// For non-sandbox containers, handleIO directly
		// Use background context since the request context may be cancelled
		if err := s.handleIO(context.Background(), c); err != nil {
			s.log.WithError(err).Warn("failed to handle IO")
		}

		if _, err := s.sandbox.StartContainer(ctx, c.id); err != nil {
			return fmt.Errorf("failed to start container: %w", err)
		}

		if err := katautils.PostStartHooks(ctx, *c.spec, s.sandbox.ID(), c.bundle); err != nil {
			s.log.WithError(err).Warn("failed to run post-start hooks")
		}
	}

	c.status = task.Status_RUNNING

	// Start background waiter goroutine - this is the key difference from kata-shim-v2
	// The Wait() API will read from exitCh instead of directly calling WaitProcess()
	go s.waitContainerProcess(c)

	s.log.WithField("container", c.id).Info("container started successfully")

	return nil
}

// waitContainerProcess runs in background to wait for container process exit
// This is similar to kata-shim-v2's wait() function in wait.go
func (s *service) waitContainerProcess(c *container) {
	// Panic recovery to prevent crashes
	defer func() {
		if r := recover(); r != nil {
			s.log.WithField("panic", r).WithField("container", c.id).Error("waitContainerProcess panic recovered")
		}
	}()

	s.log.WithField("container", c.id).Info("waitContainerProcess started")

	// Safe sandbox reference access
	s.mu.Lock()
	sandbox := s.sandbox
	s.mu.Unlock()

	if sandbox == nil {
		s.log.WithField("container", c.id).Error("sandbox is nil in waitContainerProcess")
		c.exitCh <- 255
		return
	}

	// 1. Wait for the process to exit first
	// This ensures we don't close IO streams while the process is still running
	// and potentially writing to stdout/stderr
	exitCode, err := sandbox.WaitProcess(context.Background(), c.id, c.id)
	if err != nil {
		s.log.WithError(err).WithField("container", c.id).Error("WaitProcess failed")
		// If WaitProcess fails, use exit code 255
		if exitCode == 0 {
			exitCode = 255
		}
	}

	s.log.WithField("container", c.id).WithField("exit", exitCode).Info("container process exited, waiting for IO")

	// 2. Now that process is dead, wait for IO streams to drain and close
	// The IO copy loops in handleIO should receive EOF from the sandbox streams now
	<-c.exitIOch
	s.log.WithField("container", c.id).Info("IO streams closed")

	exitTime := time.Now()

	s.mu.Lock()
	c.status = task.Status_STOPPED
	c.exit = uint32(exitCode)
	c.exitTime = exitTime
	s.mu.Unlock()

	// Send exit code to channel for Wait() to receive
	c.exitCh <- uint32(exitCode)

	// Handle sandbox cleanup for sandbox containers
	if c.cType.IsSandbox() {
		s.log.WithField("container", c.id).Debug("sandbox container exited, stopping sandbox")

		// Use a separate context for cleanup to ensure it completes even if the original context is canceled
		cleanupCtx := context.Background()

		// Safe sandbox reference access for cleanup
		s.mu.Lock()
		sandboxForCleanup := s.sandbox
		s.mu.Unlock()

		if sandboxForCleanup == nil {
			s.log.WithField("container", c.id).Warn("sandbox already nil during cleanup")
			return
		}

		// Stop the sandbox
		if err := sandboxForCleanup.Stop(cleanupCtx, true); err != nil {
			s.log.WithError(err).Warn("failed to stop sandbox")
		}

		// Delete the sandbox and clear the reference safely
		if err := sandboxForCleanup.Delete(cleanupCtx); err != nil {
			s.log.WithError(err).Warn("failed to delete sandbox")
		} else {
			s.mu.Lock()
			s.sandbox = nil
			s.mu.Unlock()
			s.log.Debug("sandbox deleted and reference cleared")
		}
	} else {
		// Safe sandbox reference access for non-sandbox container cleanup
		s.mu.Lock()
		sandboxForStop := s.sandbox
		s.mu.Unlock()

		if sandboxForStop != nil {
			if _, err := sandboxForStop.StopContainer(context.Background(), c.id, true); err != nil {
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
	s.log.WithField("container", c.id).Debug("handleIO: attempting to acquire lock")

	// Use mutex to prevent concurrent handleIO calls
	c.ioMu.Lock()
	s.log.WithField("container", c.id).Debug("handleIO: lock acquired")

	if c.ioAttached {
		c.ioMu.Unlock()
		s.log.WithField("container", c.id).Debug("IO already attached, skipping")
		return nil // Already attached
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
		// Close exitIOch immediately since there's no IO to wait for
		close(c.exitIOch)
		return nil
	}

	// Get IO streams from sandbox
	// IOStream returns (stdin, stdout, stderr, error)
	s.log.WithField("container", c.id).Debug("handleIO: calling IOStream")
	stdinStream, stdoutStream, stderrStream, err := s.sandbox.IOStream(c.id, c.id)
	s.log.WithField("container", c.id).Debug("handleIO: IOStream returned")
	if err != nil {
		c.ioMu.Unlock()
		return fmt.Errorf("failed to get IO stream: %w", err)
	}

	// Mark as attached before releasing the lock
	c.ioAttached = true

	// Create a cancellable context for IO operations
	ioCtx, ioCancel := context.WithCancel(ctx)
	c.ioCancel = ioCancel
	c.ioMu.Unlock()

	s.log.WithField("container", c.id).
		WithField("stdin", c.stdin).
		WithField("stdout", c.stdout).
		WithField("stderr", c.stderr).
		Info("attaching IO streams")

	// Like kata-shim-v2's ioCopy: when stdout/stderr completes (process exits),
	// we need to close stdin FIFO to unblock the stdin copy goroutine.

	// Handle Stdin using fifo library - open first so we can close it later
	var stdinFifo io.ReadCloser
	if c.stdin != "" && stdinStream != nil {
		// Save the stdin stream for CloseIO
		c.stdinCloser = stdinStream

		s.log.WithField("path", c.stdin).Debug("opening stdin fifo")
		// Use O_RDONLY|O_NONBLOCK for stdin to avoid blocking on open
		f, err := fifo.OpenFifo(ioCtx, c.stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			s.log.WithError(err).WithField("path", c.stdin).Warn("failed to open stdin fifo")
		} else {
			stdinFifo = f
			c.stdinFifo = f // Save for closing when stdout/stderr done
			s.log.WithField("path", c.stdin).Debug("stdin fifo opened")

			c.ioWg.Add(1)
			go func() {
				defer c.ioWg.Done()
				if _, err := io.Copy(stdinStream, stdinFifo); err != nil {
					if err != context.Canceled {
						s.log.WithError(err).Debug("stdin copy ended")
					}
				}
				s.log.WithField("container", c.id).Debug("stdin copy goroutine exited")
				// Close stdin on the sandbox side when we are done
				stdinStream.Close()
			}()
		}
	}

	// Handle Stdout using fifo library
	if c.stdout != "" && stdoutStream != nil {
		c.ioWg.Add(1)
		go func() {
			defer c.ioWg.Done()
			s.log.WithField("path", c.stdout).Info("opening stdout fifo")
			// Use O_RDWR like kata-shim-v2 to avoid blocking on open
			// O_RDWR allows the FIFO to be opened immediately without waiting for a reader
			f, err := fifo.OpenFifo(ioCtx, c.stdout, syscall.O_RDWR, 0)
			if err != nil {
				s.log.WithError(err).WithField("path", c.stdout).Warn("failed to open stdout fifo")
				return
			}
			s.log.WithField("path", c.stdout).Info("stdout fifo opened, starting copy")
			defer f.Close()

			// Copy stdout
			n, err := io.Copy(f, stdoutStream)
			s.log.WithField("bytes", n).WithField("path", c.stdout).Info("stdout copy completed")
			if err != nil {
				if err != context.Canceled {
					s.log.WithError(err).Debug("stdout copy ended with error")
				}
			}

			// Like kata-shim-v2: when stdout completes, close stdin to unblock stdin goroutine
			if stdinFifo != nil {
				s.log.WithField("container", c.id).Debug("stdout done, closing stdin fifo")
				stdinFifo.Close()
			}
		}()
	}

	// Handle Stderr using fifo library
	if c.stderr != "" && stderrStream != nil {
		c.ioWg.Add(1)
		go func() {
			defer c.ioWg.Done()
			s.log.WithField("path", c.stderr).Debug("opening stderr fifo")
			// Use O_RDWR like kata-shim-v2 to avoid blocking on open
			f, err := fifo.OpenFifo(ioCtx, c.stderr, syscall.O_RDWR, 0)
			if err != nil {
				s.log.WithError(err).WithField("path", c.stderr).Warn("failed to open stderr fifo")
				return
			}
			s.log.WithField("path", c.stderr).Debug("stderr fifo opened")
			defer f.Close()

			if _, err := io.Copy(f, stderrStream); err != nil {
				if err != context.Canceled {
					s.log.WithError(err).Debug("stderr copy ended")
				}
			}
		}()
	}

	// Start a goroutine to wait for all IO to complete and then signal exitIOch
	go func() {
		c.ioWg.Wait()
		s.log.WithField("container", c.id).Debug("all IO streams closed")
		close(c.exitIOch)
	}()

	return nil
}
