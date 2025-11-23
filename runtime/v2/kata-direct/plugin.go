// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package katadirect

import (
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/runtime/v2/shim"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.TTRPCPlugin,
		ID:   "task-kata-direct",
		Requires: []plugin.Type{
			plugin.EventPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ep, err := ic.GetByID(plugin.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}

			publisher, ok := ep.(shim.Publisher)
			if !ok {
				return nil, plugin.ErrSkipPlugin
			}

			service, err := New(ic.Context, ic.ID, publisher, func() {})
			if err != nil {
				return nil, err
			}

			return &taskService{service}, nil
		},
	})
}

type taskService struct {
	shim.Shim
}

func (t taskService) RegisterTTRPC(s interface{}) error {
	return nil
}
