// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/api/types/task"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
)

func (s *service) startContainer(ctx context.Context, c *container) error {
	if s.sandbox == nil {
		return fmt.Errorf("sandbox not found for container %s", c.id)
	}

	serviceLog.WithField("container", c.id).WithField("type", c.cType).Info("starting container")

	if c.cType.IsSandbox() {
		if err := s.sandbox.Start(ctx); err != nil {
			return fmt.Errorf("failed to start sandbox: %w", err)
		}

		monitor, err := s.sandbox.Monitor(ctx)
		if err != nil {
			serviceLog.WithError(err).Warn("failed to start sandbox monitor")
		} else {
			go s.watchSandbox(ctx, monitor)
		}

		if err := katautils.EnterNetNS(s.sandbox.GetNetNs(), func() error {
			return katautils.PostStartHooks(ctx, *c.spec, s.sandbox.ID(), c.bundle)
		}); err != nil {
			serviceLog.WithError(err).Warn("failed to run post-start hooks")
		}

	} else {
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
