/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package v2

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime"
	"github.com/containerd/log"
)

const (
	// Default pool size for warm shims
	defaultWarmPoolSize = 2
	// Default timeout for taking a shim from pool
	defaultTakeTimeout = 100 * time.Millisecond
)

// WarmPoolConfig configures warm shim pool behavior
type WarmPoolConfig struct {
	// Size is the number of warm shims to maintain
	Size int
	// TakeTimeout is how long to wait for a warm shim
	TakeTimeout time.Duration
	// Enabled controls whether warm pool is active
	Enabled bool
}

// warmPool maintains a pool of pre-started shim processes
type warmPool struct {
	runtime string
	ns      string
	state   string
	config  WarmPoolConfig
	shims   chan *warmShimInstance
	mu      sync.Mutex
	closed  bool
	manager *ShimManager
}

// warmShimInstance wraps a shim that has been warm-started
type warmShimInstance struct {
	*shim
	state   ShimState
	mu      sync.Mutex
	warmID  string
	boundID string
}

var _ WarmShim = (*warmShimInstance)(nil)

func (w *warmShimInstance) State() ShimState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *warmShimInstance) setState(state ShimState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = state
}

// Bind binds the warm shim to a specific container
func (w *warmShimInstance) Bind(ctx context.Context, id string, opts runtime.CreateOpts) error {
	w.mu.Lock()
	if w.state != ShimStateWarming {
		w.mu.Unlock()
		return fmt.Errorf("warm shim not in warming state: %v", w.state)
	}
	w.boundID = id
	w.mu.Unlock()

	log.G(ctx).WithFields(log.Fields{
		"warm_id":  w.warmID,
		"bound_id": id,
		"bundle":   opts.Spec,
	}).Info("binding warm shim to container")

	// Call the shim's Bind RPC to relocate bundle, logs, socket
	if err := w.callBindRPC(ctx, id, opts); err != nil {
		return fmt.Errorf("failed to call bind RPC: %w", err)
	}

	w.setState(ShimStateBound)

	// Update the bundle reference to point to the real bundle location
	// Calculate path same way as in callBindRPC
	warmBundleDir := filepath.Dir(w.shim.bundle.Path)
	nsDir := filepath.Dir(warmBundleDir)
	stateDir := filepath.Dir(nsDir)
	realBundle := filepath.Join(stateDir, w.shim.bundle.Namespace, id)

	w.shim.bundle.ID = id
	w.shim.bundle.Path = realBundle

	log.G(ctx).WithFields(log.Fields{
		"id":          id,
		"bundle_path": realBundle,
	}).Info("warm shim successfully bound")
	return nil
}

// callBindRPC makes the actual RPC call to the warm shim
func (w *warmShimInstance) callBindRPC(ctx context.Context, id string, opts runtime.CreateOpts) error {
	// Calculate the correct bundle path
	// Current path: /run/containerd/.../warm/default/warm-xxx
	// Target path:  /run/containerd/.../default/xxx (the real container bundle)
	//
	// We need to go up to the parent of namespace dir, then down to the real location
	warmBundleDir := filepath.Dir(w.shim.bundle.Path) // .../warm/default
	nsDir := filepath.Dir(warmBundleDir)              // .../warm
	stateDir := filepath.Dir(nsDir)                   // .../io.containerd.runtime.v2.task
	realBundle := filepath.Join(stateDir, w.shim.bundle.Namespace, id)

	req := &WarmBindRequest{
		ID:          id,
		Bundle:      realBundle,
		Rootfs:      opts.Rootfs,
		Stdin:       opts.IO.Stdin,
		Stdout:      opts.IO.Stdout,
		Stderr:      opts.IO.Stderr,
		Terminal:    opts.IO.Terminal,
		RuntimeOpts: opts.RuntimeOptions,
	}

	log.G(ctx).WithFields(log.Fields{
		"request_id":     req.ID,
		"request_bundle": req.Bundle,
	}).Debug("sending bind request to warm shim")

	// Create warm client
	warmClient, err := NewWarmClient(w.shim.client)
	if err != nil {
		return fmt.Errorf("failed to create warm client: %w", err)
	}

	// Call Bind RPC
	resp, err := warmClient.Bind(ctx, req)
	if err != nil {
		return fmt.Errorf("bind RPC failed: %w", err)
	}

	if !resp.Ready {
		return fmt.Errorf("warm shim not ready after bind")
	}

	log.G(ctx).Info("warm shim bind RPC completed successfully")
	return nil
}

// ID returns the current ID (warm or bound)
func (w *warmShimInstance) ID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.boundID != "" {
		return w.boundID
	}
	return w.warmID
}

// newWarmPool creates a new warm shim pool
func newWarmPool(ctx context.Context, manager *ShimManager, runtime, ns string, config WarmPoolConfig) *warmPool {
	_ = ctx // Reserved for future use in logging/tracing

	if config.Size <= 0 {
		config.Size = defaultWarmPoolSize
	}
	if config.TakeTimeout <= 0 {
		config.TakeTimeout = defaultTakeTimeout
	}

	pool := &warmPool{
		runtime: runtime,
		ns:      ns,
		state:   manager.state,
		config:  config,
		shims:   make(chan *warmShimInstance, config.Size),
		manager: manager,
	}

	return pool
}

// Start starts the warm pool and pre-warms shims
func (pool *warmPool) Start(ctx context.Context) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	if pool.closed {
		return fmt.Errorf("pool is closed")
	}

	log.G(ctx).WithFields(log.Fields{
		"runtime": pool.runtime,
		"ns":      pool.ns,
		"size":    pool.config.Size,
	}).Info("starting warm shim pool")

	// Pre-warm the configured number of shims
	for i := 0; i < pool.config.Size; i++ {
		if err := pool.warmOne(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("failed to warm shim")
		}
	}

	return nil
}

// warmOne creates a single warm shim instance
func (pool *warmPool) warmOne(ctx context.Context) error {
	warmID := fmt.Sprintf("warm-%s-%d", pool.ns, time.Now().UnixNano())

	logger := log.G(ctx).WithFields(log.Fields{
		"warm_id": warmID,
		"runtime": pool.runtime,
	})
	logger.Debug("creating warm shim instance")

	// Create bundle directory for warm shim
	warmBundlePath := filepath.Join(pool.state, "warm", pool.ns, warmID)
	if err := os.MkdirAll(warmBundlePath, 0700); err != nil {
		return fmt.Errorf("failed to create warm bundle dir: %w", err)
	}

	// Create minimal bundle structure
	bundle := &Bundle{
		ID:        warmID,
		Path:      warmBundlePath,
		Namespace: pool.ns,
	}

	// Start shim in warm mode
	// This would call the shim binary with "warmstart" action
	warmShim, err := pool.startWarmShim(ctx, bundle)
	if err != nil {
		os.RemoveAll(warmBundlePath)
		return fmt.Errorf("failed to start warm shim: %w", err)
	}

	w := &warmShimInstance{
		shim:   warmShim,
		state:  ShimStateWarming,
		warmID: warmID,
	}

	// Add to pool (non-blocking)
	select {
	case pool.shims <- w:
		logger.Info("warm shim added to pool")
		return nil
	default:
		// Pool is full, close this shim
		warmShim.Close()
		os.RemoveAll(warmBundlePath)
		return fmt.Errorf("pool is full")
	}
}

// startWarmShim starts a shim in warm mode
func (pool *warmPool) startWarmShim(ctx context.Context, bundle *Bundle) (*shim, error) {
	// Similar to manager.startShim but calls warmstart action
	runtimePath, err := pool.manager.resolveRuntimePath(pool.runtime)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve runtime path: %w", err)
	}

	b := shimBinary(bundle, shimBinaryConfig{
		runtime:      runtimePath,
		address:      pool.manager.containerdAddress,
		ttrpcAddress: pool.manager.containerdTTRPCAddress,
		schedCore:    pool.manager.schedCore,
	})

	// Use warmstart instead of start
	shim, err := b.StartWarm(ctx, func() {
		log.G(ctx).WithField("warm_id", bundle.ID).Info("warm shim disconnected")
	})
	if err != nil {
		return nil, fmt.Errorf("warm start failed: %w", err)
	}

	return shim, nil
}

// Take retrieves a warm shim from the pool
func (pool *warmPool) Take(ctx context.Context) *warmShimInstance {
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		return nil
	}
	pool.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, pool.config.TakeTimeout)
	defer cancel()

	select {
	case shim := <-pool.shims:
		log.G(ctx).WithField("warm_id", shim.warmID).Info("took warm shim from pool")
		// Async refill
		go func() {
			time.Sleep(100 * time.Millisecond)
			refillCtx := namespaces.WithNamespace(context.Background(), pool.ns)
			if err := pool.warmOne(refillCtx); err != nil {
				log.G(refillCtx).WithError(err).Warn("failed to refill warm pool")
			}
		}()
		return shim
	case <-ctx.Done():
		log.G(ctx).Debug("timeout waiting for warm shim")
		return nil
	}
}

// Close closes the warm pool and cleans up all warm shims
func (pool *warmPool) Close() error {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	if pool.closed {
		return nil
	}
	pool.closed = true
	close(pool.shims)

	// Clean up all remaining warm shims
	for shim := range pool.shims {
		shim.Close()
		os.RemoveAll(shim.bundle.Path)
	}

	return nil
}
