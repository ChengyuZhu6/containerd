// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/containerd/containerd/events/exchange"
	runtimeoptions "github.com/containerd/containerd/pkg/runtimeoptions/v1"
	"github.com/containerd/containerd/plugin"
	cdruntime "github.com/containerd/containerd/runtime"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/typeurl/v2"
)

var ignoreSIGHUPOnce sync.Once

func ignoreSIGHUP() {
	ignoreSIGHUPOnce.Do(func() {
		signal.Ignore(syscall.SIGHUP)
		fmt.Fprintf(os.Stderr, "[kata-direct] SIGHUP signal ignored to protect containerd from PTY closure\n")
	})
}

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.RuntimePluginV2,
		ID:   "kata-direct",
		Requires: []plugin.Type{
			plugin.EventPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {

			ignoreSIGHUP()

			ep, err := ic.GetByID(plugin.EventPlugin, "exchange")
			if err != nil {
				return nil, err
			}

			ex, ok := ep.(*exchange.Exchange)
			if !ok {
				return nil, plugin.ErrSkipPlugin
			}

			publisher := &publisherWrapper{Exchange: ex}

			return &taskServiceFactory{
				publisher: publisher,
				services:  make(map[string]shim.Shim),
			}, nil
		},
	})
}

type publisherWrapper struct {
	*exchange.Exchange
}

func (p *publisherWrapper) Close() error {
	return nil
}

var _ shim.Publisher = (*publisherWrapper)(nil)

type taskServiceFactory struct {
	mu        sync.Mutex
	publisher shim.Publisher
	services  map[string]shim.Shim
}

func (f *taskServiceFactory) ID() string {
	return "kata-direct-factory"
}

func (f *taskServiceFactory) Namespace() string {
	return ""
}

func (f *taskServiceFactory) Bundle() string {
	return ""
}

func (f *taskServiceFactory) Client() any {
	return nil
}

func (f *taskServiceFactory) Delete(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var lastErr error
	for id, svc := range f.services {
		if _, err := svc.Cleanup(ctx); err != nil {
			lastErr = fmt.Errorf("failed to cleanup service %s: %w", id, err)
		}
		delete(f.services, id)
	}
	return lastErr
}

func (f *taskServiceFactory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	ctx, cancel := withCleanupTimeout()
	defer cancel()

	var lastErr error
	for id, svc := range f.services {
		if _, err := svc.Cleanup(ctx); err != nil {
			lastErr = fmt.Errorf("failed to close service %s: %w", id, err)
		}
	}
	f.services = make(map[string]shim.Shim)
	return lastErr
}

func (f *taskServiceFactory) CreateService(ctx context.Context, id string, opts cdruntime.CreateOpts) (shim.Shim, error) {
	serviceOpts, err := buildServiceOptions(opts)
	if err != nil {
		return nil, err
	}

	svc, err := New(ctx, id, f.publisher, func() {
		f.mu.Lock()
		delete(f.services, id)
		f.mu.Unlock()
	}, serviceOpts)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	f.services[id] = svc
	f.mu.Unlock()

	return svc, nil
}

func buildServiceOptions(opts cdruntime.CreateOpts) (*serviceOptions, error) {
	serviceOpts := &serviceOptions{}

	cfgPath, err := configPathFromAny(opts.RuntimeOptions)
	if err != nil {
		return nil, err
	}
	if cfgPath == "" {
		cfgPath, err = configPathFromAny(opts.TaskOptions)
		if err != nil {
			return nil, err
		}
	}

	serviceOpts.configPath = cfgPath
	return serviceOpts, nil
}

func configPathFromAny(any typeurl.Any) (string, error) {
	if any == nil {
		return "", nil
	}

	value, err := typeurl.UnmarshalAny(any)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal runtime options: %w", err)
	}

	switch opts := value.(type) {
	case *runtimeoptions.Options:
		return opts.ConfigPath, nil
	default:
		return "", nil
	}
}
