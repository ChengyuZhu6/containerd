/*
Package v2 provides runtime v2 shim utilities, including optional prewarm pool management.
This file defines a minimal ShimPool skeleton guarded by a feature gate.
*/
package v2

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/containerd/log"
)

// Feature gate: enable shim prewarm pool management.
// Set CONTAINERD_SHIM_PREWARM_POOL=1 to enable pool-related logic by callers.
func prewarmPoolEnabled() bool {
	return os.Getenv("CONTAINERD_SHIM_PREWARM_POOL") == "1"
}

// PoolItem represents a prewarmed shim instance managed by the pool.
type PoolItem struct {
	// Namespace that this shim belongs to when adopted.
	Namespace string
	// Runtime name (e.g., io.containerd.kata.v2).
	Runtime string
	// Shim process endpoint (ttrpc or grpc address).
	Address string
	// Shim process PID if known.
	PID int
	// Whether the shim is currently idle (ready to Adopt).
	Idle bool
	// Last activity timestamp for health/timeout management.
	LastActive time.Time
	// Underlying client connection; type can be *ttrpc.Client or grpcConn.
	client any
}

// ShimPool maintains prewarmed shim instances per namespace.
type ShimPool struct {
	mu    sync.Mutex
	items map[string][]*PoolItem // namespace -> items
}

// NewShimPool creates a new empty pool. Callers should guard usage with feature gate.
func NewShimPool() *ShimPool {
	return &ShimPool{
		items: make(map[string][]*PoolItem),
	}
}

// Register adds a prewarmed shim into the pool under given namespace.
func (p *ShimPool) Register(ctx context.Context, ns string, item *PoolItem) {
	if p == nil || item == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	item.Namespace = ns
	item.Idle = true
	item.LastActive = time.Now()
	p.items[ns] = append(p.items[ns], item)
	log.G(ctx).WithFields(log.Fields{
		"namespace": ns,
		"runtime":   item.Runtime,
		"address":   item.Address,
		"pid":       item.PID,
	}).Info("registered prewarmed shim into pool")
}

// GetIdle returns an idle prewarmed shim for namespace/runtime, or nil if none.
func (p *ShimPool) GetIdle(ctx context.Context, ns, runtime string) *PoolItem {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.items[ns]
	for _, it := range list {
		if it.Idle && (runtime == "" || it.Runtime == runtime) {
			it.Idle = false
			it.LastActive = time.Now()
			log.G(ctx).WithFields(log.Fields{
				"namespace": ns,
				"runtime":   it.Runtime,
				"address":   it.Address,
				"pid":       it.PID,
			}).Info("checked out prewarmed shim from pool")
			return it
		}
	}
	return nil
}

// Return marks a pool item idle again after container lifecycle finishes.
func (p *ShimPool) Return(ctx context.Context, item *PoolItem) {
	if p == nil || item == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	item.Idle = true
	item.LastActive = time.Now()
	log.G(ctx).WithFields(log.Fields{
		"namespace": item.Namespace,
		"runtime":   item.Runtime,
		"address":   item.Address,
		"pid":       item.PID,
	}).Info("returned shim to prewarm pool")
}

// Remove deletes a pool item (e.g., on failure or unhealthy state).
func (p *ShimPool) Remove(ctx context.Context, ns string, item *PoolItem) {
	if p == nil || item == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	items := p.items[ns]
	out := items[:0]
	for _, it := range items {
		if it != item {
			out = append(out, it)
		}
	}
	p.items[ns] = out
	log.G(ctx).WithFields(log.Fields{
		"namespace": ns,
		"runtime":   item.Runtime,
		"address":   item.Address,
		"pid":       item.PID,
	}).Warn("removed shim from prewarm pool")
}

// Sweep removes idle shims that exceeded a given idle timeout.
// Callers can schedule this based on health checks.
func (p *ShimPool) Sweep(ctx context.Context, ns string, idleTimeout time.Duration) (removed int) {
	if p == nil {
		return 0
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	items := p.items[ns]
	out := items[:0]
	for _, it := range items {
		if it.Idle && idleTimeout > 0 && now.Sub(it.LastActive) > idleTimeout {
			removed++
			log.G(ctx).WithFields(log.Fields{
				"namespace": ns,
				"runtime":   it.Runtime,
				"address":   it.Address,
				"pid":       it.PID,
			}).Warn("sweeping idle shim from prewarm pool due to timeout")
			// TODO: optionally close connection and kill process if needed
			continue
		}
		out = append(out, it)
	}
	p.items[ns] = out
	return removed
}
