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

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
)

// WarmClient provides client interface for calling warm shim Bind RPC
type WarmClient interface {
	Bind(ctx context.Context, req *WarmBindRequest) (*WarmBindResponse, error)
}

// warmClientImpl implements WarmClient using ttrpc
type warmClientImpl struct {
	client any // *ttrpc.Client or grpc client
}

// NewWarmClient creates a client for calling warm shim operations
func NewWarmClient(shimClient any) (WarmClient, error) {
	if shimClient == nil {
		return nil, fmt.Errorf("shim client is nil")
	}

	return &warmClientImpl{
		client: shimClient,
	}, nil
}

// Bind calls the Bind RPC on the warm shim
func (c *warmClientImpl) Bind(ctx context.Context, req *WarmBindRequest) (*WarmBindResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"id":     req.ID,
		"bundle": req.Bundle,
	}).Debug("calling warm shim bind RPC")

	// For prototype: since we don't have actual proto definitions,
	// we simulate the RPC call with logging
	// In production, this would be a real ttrpc call like:
	// client := shimWarmService.NewWarmClient(c.client.(*ttrpc.Client))
	// return client.Bind(ctx, req)

	if ttrpcClient, ok := c.client.(*ttrpc.Client); ok {
		_ = ttrpcClient // Use the client to prevent unused warning

		// Simulate successful bind for prototype
		log.G(ctx).Info("warm bind RPC called (prototype mode)")
		return &WarmBindResponse{
			Ready: true,
		}, nil
	}

	// For GRPC clients, similar approach would be used
	log.G(ctx).Info("warm bind RPC called (prototype mode, grpc)")
	return &WarmBindResponse{
		Ready: true,
	}, nil
}
