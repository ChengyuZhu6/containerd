// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
)

func (s *service) startContainer(ctx context.Context, c *container) error {
	if s.sandbox == nil {
		return fmt.Errorf("sandbox not found for container %s", c.id)
	}

	serviceLog.WithField("container", c.id).WithField("type", c.cType).Info("starting container")

	if c.cType.IsSandbox() {
		ioAttached := make(chan struct{})

		go func() {
			ticker := time.NewTicker(1 * time.Millisecond)
			defer ticker.Stop()

			// Increase timeout to accommodate slow startup
			timeout := time.After(5 * time.Second)

			for {
				select {
				case <-s.ctx.Done():
					return
				case <-timeout:
					serviceLog.Warn("timeout waiting for IO stream")
					return
				case <-ticker.C:
					// Try to get IO stream.
					// For Sandbox container, we need to wait until it's running.
					if err := s.handleIO(s.ctx, c); err == nil {
						close(ioAttached)
						return
					}
				}
			}
		}()

		if err := s.sandbox.Start(ctx); err != nil {
			return fmt.Errorf("failed to start sandbox: %w", err)
		}

		// Wait for IO to be attached or timeout
		// This ensures that if the process is short-lived, we have a high chance
		// of attaching IO before returning, so that Delete() will wait for the output.
		select {
		case <-ioAttached:
		case <-time.After(2 * time.Second):
			serviceLog.Warn("proceeding without IO attached (timeout)")
		}

		monitor, err := s.sandbox.Monitor(s.ctx)
		if err != nil {
			serviceLog.WithError(err).Warn("failed to start sandbox monitor")
		} else {
			go s.watchSandbox(s.ctx, monitor)
		}

		if err := katautils.EnterNetNS(s.sandbox.GetNetNs(), func() error {
			return katautils.PostStartHooks(ctx, *c.spec, s.sandbox.ID(), c.bundle)
		}); err != nil {
			serviceLog.WithError(err).Warn("failed to run post-start hooks")
		}

	} else {
		if err := s.handleIO(s.ctx, c); err != nil {
			serviceLog.WithError(err).Warn("failed to handle IO")
		}

		if _, err := s.sandbox.StartContainer(ctx, c.id); err != nil {
			return fmt.Errorf("failed to start container: %w", err)
		}

		if err := katautils.PostStartHooks(ctx, *c.spec, s.sandbox.ID(), c.bundle); err != nil {
			serviceLog.WithError(err).Warn("failed to run post-start hooks")
		}
	}

	c.status = task.Status_RUNNING

	serviceLog.WithField("container", c.id).Info("container started successfully")

	return nil
}

func (s *service) watchSandbox(ctx context.Context, monitor chan error) {
	defer func() {
		if r := recover(); r != nil {
			serviceLog.WithField("panic", r).Error("watchSandbox panic recovered")
		}
	}()

	select {
	case err := <-monitor:
		if err != nil {
			serviceLog.WithError(err).Error("sandbox monitor error")
		} else {
			serviceLog.Info("sandbox exited normally")
		}
	case <-ctx.Done():
		serviceLog.Info("sandbox monitor stopped")
	}
}

func (s *service) handleIO(ctx context.Context, c *container) error {
	// If no IO paths are provided, do nothing
	if c.stdin == "" && c.stdout == "" && c.stderr == "" {
		return nil
	}

	// Get IO streams from sandbox
	// IOStream returns (stdin, stdout, stderr, error)
	stdin, stdout, stderr, err := s.sandbox.IOStream(c.id, c.id)
	if err != nil {
		return fmt.Errorf("failed to get IO stream: %w", err)
	}

	// Handle Stdout
	if c.stdout != "" && stdout != nil {
		c.ioWg.Add(1)
		go func() {
			defer c.ioWg.Done()
			// Open the fifo/file provided by containerd
			f, err := os.OpenFile(c.stdout, os.O_RDWR, 0755)
			if err != nil {
				serviceLog.WithError(err).WithField("path", c.stdout).Warn("failed to open stdout file")
				return
			}
			defer f.Close()

			if _, err := io.Copy(f, stdout); err != nil {
				serviceLog.WithError(err).Warn("failed to copy stdout")
			}
		}()
	}

	// Handle Stderr
	if c.stderr != "" && stderr != nil {
		c.ioWg.Add(1)
		go func() {
			defer c.ioWg.Done()
			f, err := os.OpenFile(c.stderr, os.O_RDWR, 0755)
			if err != nil {
				serviceLog.WithError(err).WithField("path", c.stderr).Warn("failed to open stderr file")
				return
			}
			defer f.Close()

			if _, err := io.Copy(f, stderr); err != nil {
				serviceLog.WithError(err).Warn("failed to copy stderr")
			}
		}()
	}

	// Handle Stdin
	if c.stdin != "" && stdin != nil {
		c.ioWg.Add(1)
		go func() {
			defer c.ioWg.Done()
			f, err := os.OpenFile(c.stdin, os.O_RDWR, 0755)
			if err != nil {
				serviceLog.WithError(err).WithField("path", c.stdin).Warn("failed to open stdin file")
				return
			}
			defer f.Close()

			if _, err := io.Copy(stdin, f); err != nil {
				serviceLog.WithError(err).Warn("failed to copy stdin")
			}
			// Close stdin on the sandbox side when we are done
			stdin.Close()
		}()
	}

	// Ensure we wait for IO to finish in the background, but we don't block handleIO
	// The waitgroup is attached to the container structure.

	return nil
}
