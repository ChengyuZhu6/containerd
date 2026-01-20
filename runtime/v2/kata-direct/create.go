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

	s.log.WithField("type", containerType).WithField("id", r.ID).Info("creating container")

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
		id:       r.ID,
		bundle:   bundlePath,
		spec:     &ociSpec,
		mounted:  rootFs.Mounted,
		cType:    containerType,
		stdin:    r.Stdin,
		stdout:   r.Stdout,
		stderr:   r.Stderr,
		terminal: r.Terminal,
		// Initialize channels for exit handling - like kata-shim-v2
		exitCh:   make(chan uint32, 1),
		exitIOch: make(chan struct{}),
	}

	s.log.WithField("container", r.ID).
		WithField("stdin", r.Stdin).
		WithField("stdout", r.Stdout).
		WithField("stderr", r.Stderr).
		WithField("terminal", r.Terminal).
		Info("container IO config")

	switch containerType {
	case vc.PodSandbox, vc.SingleContainer:
		if err := s.createSandbox(ctx, r.ID, bundlePath, &ociSpec, rootFs); err != nil {
			if rootFs.Mounted {
				mount.UnmountAll(rootfs, 0)
			}
			return nil, fmt.Errorf("failed to create sandbox: %w", err)
		}

	case vc.PodContainer:
		// Use getSandbox() to safely access sandbox under RLock
		sandbox := s.getSandbox()
		if sandbox == nil {
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
	// Load configuration under lock to avoid data race
	s.mu.Lock()
	if s.config == nil {
		configPath := oci.GetSandboxConfigPath(ociSpec.Annotations)
		if configPath == "" {
			configPath = s.configPath
		}
		s.log.WithField("config", configPath).Info("loading kata configuration")

		_, runtimeConfig, err := katautils.LoadConfiguration(configPath, false)
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("failed to load kata configuration: %w", err)
		}
		s.config = &runtimeConfig
	}

	s.config.SandboxCPUs, s.config.SandboxMemMB = oci.CalculateSandboxSizing(ociSpec)
	// Copy config for use outside lock
	configCopy := *s.config
	s.mu.Unlock()

	s.log.WithField("cpus", configCopy.SandboxCPUs).
		WithField("memory_mb", configCopy.SandboxMemMB).
		Info("sandbox sizing calculated")

	// Use context.Background() for sandbox creation because:
	// 1. sandbox.ctx is stored and used by kataAgent for all RPC calls
	// 2. The request context (ctx) will be cancelled when the Create RPC completes
	// 3. Long-running operations like readProcessStdout use k.ctx which comes from sandbox.ctx
	// Using request context would cause "context canceled" errors when reading container output
	//
	// NOTE: This is a long-running operation performed WITHOUT holding the lock
	// to allow concurrent read operations (State, Pids, etc.)
	sandbox, _, err := katautils.CreateSandbox(
		context.Background(),
		s.vci,
		*ociSpec,
		configCopy,
		rootFs,
		id,
		bundlePath,
		false,
		false,
	)
	if err != nil {
		return fmt.Errorf("failed to create kata sandbox: %w", err)
	}

	// CRITICAL FIX: Acquire lock to safely assign sandbox and check for cleanup race condition
	// This prevents data race with Cleanup() and other methods that access s.sandbox/s.hpid
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check for race condition: if Cleanup() was called during sandbox creation,
	// we must destroy the newly created sandbox to prevent resource leakage
	if s.cleaned {
		s.log.Warn("service cleanup triggered during sandbox creation, destroying orphan sandbox")
		// Destroy the orphan sandbox in background to avoid blocking
		go func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
			defer cancel()
			if err := sandbox.Stop(cleanupCtx, true); err != nil {
				s.log.WithError(err).Error("failed to stop orphan sandbox")
			}
			if err := sandbox.Delete(cleanupCtx); err != nil {
				s.log.WithError(err).Error("failed to delete orphan sandbox")
			}
		}()
		return fmt.Errorf("service cleanup triggered during sandbox creation")
	}

	// Safe to assign now - we hold the lock and cleanup hasn't been triggered
	s.sandbox = sandbox

	pid, err := sandbox.GetHypervisorPid()
	if err != nil {
		s.log.WithError(err).Warn("failed to get hypervisor pid")
		s.hpid = uint32(os.Getpid())
	} else {
		s.hpid = uint32(pid)
		s.log.WithField("hypervisor_pid", pid).Info("hypervisor started")
	}

	return nil
}

func (s *service) createPodContainer(ctx context.Context, id, bundlePath string, ociSpec *specs.Spec, rootFs vc.RootFs) error {
	// Safely get sandbox reference under lock
	sandbox := s.getSandbox()
	if sandbox == nil {
		return fmt.Errorf("sandbox not found for container %s", id)
	}

	// Use context.Background() for container creation to be consistent with sandbox creation
	// This ensures long-running operations don't get cancelled when the request completes
	_, err := katautils.CreateContainer(
		context.Background(),
		sandbox,
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
