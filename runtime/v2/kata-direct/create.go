// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/mount"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/oci"
	vc "github.com/kata-containers/kata-containers/src/runtime/virtcontainers"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/compatoci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func (s *service) createContainer(ctx context.Context, r *taskAPI.CreateTaskRequest) (*container, error) {
	bundlePath := r.Bundle
	ociSpec, err := compatoci.ParseConfigJSON(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config.json: %w", err)
	}

	containerType, err := oci.ContainerType(ociSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to get container type: %w", err)
	}

	serviceLog.WithField("type", containerType).WithField("id", r.ID).Info("creating container")

	rootFs := vc.RootFs{}
	if len(r.Rootfs) > 0 {
		rootFs.Source = r.Rootfs[0].Source
		rootFs.Type = r.Rootfs[0].Type
		rootFs.Options = r.Rootfs[0].Options
	}

	rootfs := filepath.Join(bundlePath, "rootfs")
	if len(r.Rootfs) > 0 {
		// Convert []*types.Mount to []mount.Mount
		mounts := make([]mount.Mount, len(r.Rootfs))
		for i, m := range r.Rootfs {
			mounts[i] = mount.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Options: m.Options,
			}
		}
		if err := mount.All(mounts, rootfs); err != nil {
			return nil, fmt.Errorf("failed to mount rootfs: %w", err)
		}
		rootFs.Mounted = true
	}

	c := &container{
		id:      r.ID,
		bundle:  bundlePath,
		spec:    &ociSpec,
		mounted: rootFs.Mounted,
		cType:   containerType,
	}

	switch containerType {
	case vc.PodSandbox, vc.SingleContainer:
		if err := s.createSandbox(ctx, r.ID, bundlePath, &ociSpec, rootFs); err != nil {
			if rootFs.Mounted {
				mount.UnmountAll(rootfs, 0)
			}
			return nil, fmt.Errorf("failed to create sandbox: %w", err)
		}

	case vc.PodContainer:
		if s.sandbox == nil {
			if rootFs.Mounted {
				mount.UnmountAll(rootfs, 0)
			}
			return nil, fmt.Errorf("sandbox not found for container %s", r.ID)
		}

		if err := s.createPodContainer(ctx, r.ID, bundlePath, &ociSpec, rootFs); err != nil {
			if rootFs.Mounted {
				mount.UnmountAll(rootfs, 0)
			}
			return nil, fmt.Errorf("failed to create pod container: %w", err)
		}

	default:
		if rootFs.Mounted {
			mount.UnmountAll(rootfs, 0)
		}
		return nil, fmt.Errorf("unknown container type: %v", containerType)
	}

	return c, nil
}

func (s *service) createSandbox(ctx context.Context, id, bundlePath string, ociSpec *specs.Spec, rootFs vc.RootFs) error {
	if s.config == nil {
		configPath := oci.GetSandboxConfigPath(ociSpec.Annotations)
		if configPath == "" {
			configPath = s.configPath
		}
		serviceLog.WithField("config", configPath).Info("loading kata configuration")

		_, runtimeConfig, err := katautils.LoadConfiguration(configPath, false)
		if err != nil {
			return fmt.Errorf("failed to load kata configuration: %w", err)
		}
		s.config = &runtimeConfig
	}

	s.config.SandboxCPUs, s.config.SandboxMemMB = oci.CalculateSandboxSizing(ociSpec)

	serviceLog.WithField("cpus", s.config.SandboxCPUs).
		WithField("memory_mb", s.config.SandboxMemMB).
		Info("sandbox sizing calculated")

	sandbox, _, err := katautils.CreateSandbox(
		ctx,
		s.vci,
		*ociSpec,
		*s.config,
		rootFs,
		id,
		bundlePath,
		false,
		false,
	)
	if err != nil {
		return fmt.Errorf("failed to create kata sandbox: %w", err)
	}

	s.sandbox = sandbox

	pid, err := sandbox.GetHypervisorPid()
	if err != nil {
		serviceLog.WithError(err).Warn("failed to get hypervisor pid")
		s.hpid = uint32(os.Getpid())
	} else {
		s.hpid = uint32(pid)
		serviceLog.WithField("hypervisor_pid", pid).Info("hypervisor started")
	}

	return nil
}

func (s *service) createPodContainer(ctx context.Context, id, bundlePath string, ociSpec *specs.Spec, rootFs vc.RootFs) error {
	_, err := katautils.CreateContainer(
		ctx,
		s.sandbox,
		*ociSpec,
		rootFs,
		id,
		bundlePath,
		false,
		false,
	)
	if err != nil {
		return fmt.Errorf("failed to create container in sandbox: %w", err)
	}

	return nil
}
