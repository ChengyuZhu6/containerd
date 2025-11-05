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

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/runtime"
	"github.com/containerd/typeurl/v2"
)

// ShimState represents the state of a shim instance
type ShimState int

const (
	// ShimStateWarming indicates shim is pre-started but not bound
	ShimStateWarming ShimState = iota
	// ShimStateBound indicates shim has been bound to a container
	ShimStateBound
	// ShimStateActive indicates container is running
	ShimStateActive
)

// WarmShim represents a warm (pre-started) shim instance
type WarmShim interface {
	ShimInstance
	// Bind binds the warm shim to a specific container
	Bind(ctx context.Context, id string, opts runtime.CreateOpts) error
	// State returns current state of the warm shim
	State() ShimState
}

// WarmBindRequest contains parameters for binding a warm shim to a container
type WarmBindRequest struct {
	ID          string
	Bundle      string
	Rootfs      []mount.Mount
	Stdin       string
	Stdout      string
	Stderr      string
	Terminal    bool
	RuntimeOpts typeurl.Any
}

// WarmBindResponse is the response from a bind operation
type WarmBindResponse struct {
	Ready bool
}
