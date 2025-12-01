// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/events/exchange"
	runtimeoptions "github.com/containerd/containerd/pkg/runtimeoptions/v1"
	"github.com/containerd/containerd/plugin"
	cdruntime "github.com/containerd/containerd/runtime"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/typeurl/v2"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.RuntimePluginV2,
		ID:   "kata-direct",
		Requires: []plugin.Type{
			plugin.EventPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ep, err := ic.GetByID(plugin.EventPlugin, "exchange")
			if err != nil {
				return nil, err
			}

			ex, ok := ep.(*exchange.Exchange)
			if !ok {
				return nil, plugin.ErrSkipPlugin
			}

			// Wrap exchange to implement shim.Publisher
			publisher := &publisherWrapper{Exchange: ex}

			// Return a factory that will create services on demand
			return &taskServiceFactory{
				publisher: publisher,
			}, nil
		},
	})
}

// publisherWrapper wraps exchange.Exchange to implement shim.Publisher
type publisherWrapper struct {
	*exchange.Exchange
}

func (p *publisherWrapper) Close() error {
	// Exchange doesn't need to be closed
	return nil
}

var _ shim.Publisher = (*publisherWrapper)(nil)

// taskServiceFactory creates kata-direct services on demand
type taskServiceFactory struct {
	publisher shim.Publisher
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
	return nil
}

func (f *taskServiceFactory) Close() error {
	return nil
}

// CreateService creates a new kata-direct service instance
func (f *taskServiceFactory) CreateService(ctx context.Context, id string, opts cdruntime.CreateOpts) (shim.Shim, error) {
	serviceOpts, err := buildServiceOptions(opts)
	if err != nil {
		return nil, err
	}

	return New(ctx, id, f.publisher, func() {}, serviceOpts)
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
