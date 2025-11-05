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

package shim

import (
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/ttrpc"
)

func init() {
	// Register warm service plugin for warm shim mode
	plugin.Register(&plugin.Registration{
		Type: plugin.TTRPCPlugin,
		ID:   "warm",
		Requires: []plugin.Type{
			plugin.EventPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			// Get bundle path from context
			opts, ok := ic.Context.Value(OptsKey{}).(Opts)
			if !ok {
				return plugin.ErrSkipPlugin, nil
			}

			// Create warm service - use "warm" as the warm ID placeholder
			svc := NewWarmService("warm", opts.BundlePath)
			return warmServiceWrapper{svc}, nil
		},
	})
}

// warmServiceWrapper wraps WarmService to implement ttrpcService interface
type warmServiceWrapper struct {
	svc WarmService
}

func (w warmServiceWrapper) RegisterTTRPC(server *ttrpc.Server) error {
	RegisterWarmService(server, w.svc)
	return nil
}
