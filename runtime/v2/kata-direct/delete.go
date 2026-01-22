// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/mount"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	"golang.org/x/sys/unix"
)

func (s *service) deleteContainer(ctx context.Context, c *container) error {
	sandbox := s.getSandbox()

	// For non-sandbox containers, sandbox must exist
	// For sandbox containers, sandbox may have been cleaned up in cleanupAfterExit
	if sandbox == nil && !c.cType.IsSandbox() {
		return fmt.Errorf("sandbox not found for container %s", c.id)
	}

	s.log.WithField("container", c.id).WithField("type", c.cType).Info("deleting container")

	// Cancel IO operations first to unblock any pending IO
	c.ioMu.Lock()
	if c.ioCancel != nil {
		c.ioCancel()
	}
	c.ioMu.Unlock()

	// Add timeout for operations
	opCtx, cancel := withOperationTimeout(ctx)
	defer cancel()

	if !c.cType.IsSandbox() {
		if c.status != task.Status_STOPPED {
			if _, err := sandbox.StopContainer(opCtx, c.id, false); err != nil {
				s.log.WithError(err).Warn("failed to stop container")
			}
		}

		// Wait for IO to finish with timeout
		ioDone := make(chan struct{})
		go func() {
			c.ioWg.Wait()
			close(ioDone)
		}()

		select {
		case <-ioDone:
			// IO finished normally
		case <-time.After(defaultCleanupTimeout):
			s.log.WithField("container", c.id).Warn("timeout waiting for IO during delete")
		}

		if _, err := sandbox.DeleteContainer(opCtx, c.id); err != nil {
			s.log.WithError(err).Warn("failed to delete container")
		}
	} else {
		c.status = task.Status_STOPPED
		c.exitTime = time.Now()
		c.exit = 128 + uint32(unix.SIGKILL)

		// Wait for IO to finish with timeout for sandbox container too
		ioDone := make(chan struct{})
		go func() {
			c.ioWg.Wait()
			close(ioDone)
		}()

		select {
		case <-ioDone:
			// IO finished normally
		case <-time.After(defaultCleanupTimeout):
			s.log.WithField("container", c.id).Warn("timeout waiting for IO during delete")
		}
	}

	// Run post-stop hooks - sandbox may be nil for sandbox containers after cleanup
	sandboxID := c.id // For sandbox containers, use container id as sandbox id
	if sandbox != nil {
		sandboxID = sandbox.ID()
	}
	if err := katautils.PostStopHooks(opCtx, *c.spec, sandboxID, c.bundle); err != nil {
		s.log.WithError(err).Warn("failed to run post-stop hooks")
	}

	if c.mounted {
		rootfs := filepath.Join(c.bundle, "rootfs")
		if err := mount.UnmountAll(rootfs, 0); err != nil {
			s.log.WithError(err).Warn("failed to unmount rootfs")
		}
	}

	// Ensure exitCh is closed so any Wait() callers are unblocked
	// This is safe to call even if waitContainerProcess already closed it (uses sync.Once)
	c.closeExitCh()

	s.log.WithField("container", c.id).Info("container deleted successfully")

	return nil
}
