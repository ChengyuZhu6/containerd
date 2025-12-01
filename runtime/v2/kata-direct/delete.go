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
	if s.sandbox == nil {
		return fmt.Errorf("sandbox not found for container %s", c.id)
	}

	serviceLog.WithField("container", c.id).WithField("type", c.cType).Info("deleting container")

	// Cancel IO operations first to unblock any pending IO
	if c.ioCancel != nil {
		c.ioCancel()
	}

	if !c.cType.IsSandbox() {
		if c.status != task.Status_STOPPED {
			if _, err := s.sandbox.StopContainer(ctx, c.id, false); err != nil {
				serviceLog.WithError(err).Warn("failed to stop container")
			}
		}

		// Wait for IO to finish
		c.ioWg.Wait()

		if _, err := s.sandbox.DeleteContainer(ctx, c.id); err != nil {
			serviceLog.WithError(err).Warn("failed to delete container")
		}
	} else {
		c.status = task.Status_STOPPED
		c.exitTime = time.Now()
		c.exit = 128 + uint32(unix.SIGKILL)
		// Wait for IO to finish for sandbox container too
		c.ioWg.Wait()
	}

	if err := katautils.PostStopHooks(ctx, *c.spec, s.sandbox.ID(), c.bundle); err != nil {
		serviceLog.WithError(err).Warn("failed to run post-stop hooks")
	}

	if c.mounted {
		rootfs := filepath.Join(c.bundle, "rootfs")
		if err := mount.UnmountAll(rootfs, 0); err != nil {
			serviceLog.WithError(err).Warn("failed to unmount rootfs")
		}
	}

	serviceLog.WithField("container", c.id).Info("container deleted successfully")

	return nil
}
