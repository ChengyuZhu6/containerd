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
	if sandbox == nil {
		return fmt.Errorf("sandbox not found for container %s", c.id)
	}

	s.log.WithField("container", c.id).WithField("type", c.cType).Info("deleting container")

	c.ioMu.Lock()
	if c.ioCancel != nil {
		c.ioCancel()
	}
	c.ioMu.Unlock()

	opCtx, cancel := withOperationTimeout(ctx)
	defer cancel()

	if !c.cType.IsSandbox() {
		if c.status != task.Status_STOPPED {
			if _, err := sandbox.StopContainer(opCtx, c.id, false); err != nil {
				s.log.WithError(err).Warn("failed to stop container")
			}
		}

		ioDone := make(chan struct{})
		go func() {
			c.ioWg.Wait()
			close(ioDone)
		}()

		select {
		case <-ioDone:
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

		ioDone := make(chan struct{})
		go func() {
			c.ioWg.Wait()
			close(ioDone)
		}()

		select {
		case <-ioDone:
		case <-time.After(defaultCleanupTimeout):
			s.log.WithField("container", c.id).Warn("timeout waiting for IO during delete")
		}
	}

	if err := katautils.PostStopHooks(opCtx, *c.spec, sandbox.ID(), c.bundle); err != nil {
		s.log.WithError(err).Warn("failed to run post-stop hooks")
	}

	if c.mounted {
		rootfs := filepath.Join(c.bundle, "rootfs")
		if err := mount.UnmountAll(rootfs, 0); err != nil {
			s.log.WithError(err).Warn("failed to unmount rootfs")
		}
	}

	s.log.WithField("container", c.id).Info("container deleted successfully")

	return nil
}
