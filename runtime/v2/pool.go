package v2

import (
	"context"
	"sync"
	"time"

	"github.com/containerd/log"
)

// PoolItem represents a prewarmed shim that can be adopted by a container.
type PoolItem struct {
	// Address the shim listens on (ttrpc/grpc).
	Address string
	// PID of the shim process (optional; used for diagnostics/health).
	PID int
	// Namespace the shim was started under.
	Namespace string
	// Runtime name (e.g., io.containerd.runc.v2).
	Runtime string
	// Idle indicates whether the shim is free to adopt a container.
	Idle bool
	// LastActive records the last time this shim processed activity.
	LastActive time.Time
}

// ShimPool maintains prewarmed shims keyed by namespace and runtime.
type ShimPool struct {
	mu sync.Mutex
	// items maps "namespace|runtime" -> list of items
	items map[string][]*PoolItem
	// index maps address -> item for quick removal/update
	index map[string]*PoolItem

	// Configuration knobs (placeholders for future tuning).
	IdleTTL       time.Duration // prune idle items after this duration without activity
	HealthTimeout time.Duration // optional future health check timeout
}

// NewShimPool creates a new shim pool with default parameters.
func NewShimPool() *ShimPool {
	return &ShimPool{
		items:         make(map[string][]*PoolItem),
		index:         make(map[string]*PoolItem),
		IdleTTL:       10 * time.Minute,
		HealthTimeout: 5 * time.Second,
	}
}

// key builds the storage key for a namespace/runtime pair.
func (p *ShimPool) key(ns, runtime string) string {
	return ns + "|" + runtime
}

// Register inserts a prewarmed shim into the pool as idle.
func (p *ShimPool) Register(ctx context.Context, ns, runtime, address string, pid int) *PoolItem {
	p.mu.Lock()
	defer p.mu.Unlock()

	item := &PoolItem{
		Address:    address,
		PID:        pid,
		Namespace:  ns,
		Runtime:    runtime,
		Idle:       true,
		LastActive: time.Now(),
	}
	k := p.key(ns, runtime)
	p.items[k] = append(p.items[k], item)
	p.index[address] = item

	log.G(ctx).WithFields(log.Fields{
		"namespace": ns,
		"runtime":   runtime,
		"address":   address,
		"pid":       pid,
	}).Info("shim pool: registered prewarmed shim")

	return item
}

// GetIdle retrieves and marks an idle shim for the given namespace/runtime.
// Returns nil if none available.
func (p *ShimPool) GetIdle(ctx context.Context, ns, runtime string) *PoolItem {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := p.key(ns, runtime)
	list := p.items[k]
	for _, it := range list {
		if it.Idle {
			it.Idle = false
			it.LastActive = time.Now()
			log.G(ctx).WithFields(log.Fields{
				"namespace": ns,
				"runtime":   runtime,
				"address":   it.Address,
				"pid":       it.PID,
			}).Info("shim pool: acquired idle prewarmed shim")
			return it
		}
	}
	return nil
}

// Return marks a shim back to idle after container lifecycle completes.
// Optionally refreshes LastActive to the current time.
func (p *ShimPool) Return(ctx context.Context, address string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if it, ok := p.index[address]; ok {
		it.Idle = true
		it.LastActive = time.Now()
		log.G(ctx).WithFields(log.Fields{
			"namespace": it.Namespace,
			"runtime":   it.Runtime,
			"address":   it.Address,
		}).Info("shim pool: returned shim to idle")
	}
}

// Remove deletes a shim from the pool, e.g. on failure or shutdown.
func (p *ShimPool) Remove(ctx context.Context, ns string, item *PoolItem) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.index, item.Address)
	k := p.key(ns, item.Runtime)
	list := p.items[k]
	out := list[:0]
	for _, it := range list {
		if it.Address != item.Address {
			out = append(out, it)
		}
	}
	if len(out) == 0 {
		delete(p.items, k)
	} else {
		p.items[k] = out
	}

	log.G(ctx).WithFields(log.Fields{
		"namespace": item.Namespace,
		"runtime":   item.Runtime,
		"address":   item.Address,
	}).Info("shim pool: removed shim from pool")
}

// Prune removes idle shims that have exceeded IdleTTL.
// Intended to be called periodically by a manager.
func (p *ShimPool) Prune(ctx context.Context, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for k, list := range p.items {
		out := list[:0]
		for _, it := range list {
			if it.Idle && p.IdleTTL > 0 && now.Sub(it.LastActive) > p.IdleTTL {
				delete(p.index, it.Address)
				log.G(ctx).WithFields(log.Fields{
					"namespace": it.Namespace,
					"runtime":   it.Runtime,
					"address":   it.Address,
				}).Info("shim pool: pruned idle shim")
				continue
			}
			out = append(out, it)
		}
		if len(out) == 0 {
			delete(p.items, k)
		} else {
			p.items[k] = out
		}
	}
}

// Len returns the total number of tracked shims in the pool.
func (p *ShimPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.index)
}
