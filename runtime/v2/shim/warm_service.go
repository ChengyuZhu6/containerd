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
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
)

// WarmService provides bind functionality for warm shims
type WarmService interface {
	// Bind binds the warm shim to a specific container bundle
	Bind(ctx context.Context, req *WarmBindRequest) (*WarmBindResponse, error)
}

// WarmBindRequest contains parameters for binding a warm shim
type WarmBindRequest struct {
	ID       string
	Bundle   string
	Rootfs   []*Mount
	Stdin    string
	Stdout   string
	Stderr   string
	Terminal bool
	Options  []byte
}

// WarmBindResponse is returned after binding
type WarmBindResponse struct {
	Ready bool
}

// Mount represents a mount point
type Mount struct {
	Type    string
	Source  string
	Target  string
	Options []string
}

// warmServiceImpl implements WarmService for shim
type warmServiceImpl struct {
	warmID     string
	boundID    string
	bundlePath string
}

// NewWarmService creates a new warm service instance
func NewWarmService(warmID, bundlePath string) WarmService {
	return &warmServiceImpl{
		warmID:     warmID,
		bundlePath: bundlePath,
	}
}

// Bind implements the binding of warm shim to a real container
func (w *warmServiceImpl) Bind(ctx context.Context, req *WarmBindRequest) (*WarmBindResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"warm_id":  w.warmID,
		"bound_id": req.ID,
		"bundle":   req.Bundle,
	}).Info("warm shim bind request received")

	// Validate request
	if req.ID == "" {
		return nil, fmt.Errorf("bind request missing container ID")
	}
	if req.Bundle == "" {
		return nil, fmt.Errorf("bind request missing bundle path")
	}

	// Create the target bundle directory if it doesn't exist
	if err := os.MkdirAll(req.Bundle, 0700); err != nil {
		return nil, fmt.Errorf("failed to create bundle directory: %w", err)
	}

	// Move address file to new bundle location
	oldAddressPath := filepath.Join(w.bundlePath, "address")
	newAddressPath := filepath.Join(req.Bundle, "address")
	if err := os.Rename(oldAddressPath, newAddressPath); err != nil {
		// If rename fails, try copy and delete
		if data, err := os.ReadFile(oldAddressPath); err == nil {
			if err := os.WriteFile(newAddressPath, data, 0600); err == nil {
				os.Remove(oldAddressPath)
			}
		}
	}

	// Update internal state
	w.boundID = req.ID
	w.bundlePath = req.Bundle

	// Change working directory to new bundle
	if err := os.Chdir(req.Bundle); err != nil {
		return nil, fmt.Errorf("failed to change to bundle directory: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"warm_id":    w.warmID,
		"bound_id":   req.ID,
		"new_bundle": req.Bundle,
	}).Info("warm shim successfully bound to container")

	return &WarmBindResponse{
		Ready: true,
	}, nil
}

// RegisterWarmService registers the warm service with ttrpc server
func RegisterWarmService(server *ttrpc.Server, svc WarmService) {
	// For prototype, we skip actual ttrpc registration
	// In production, this would use proper ttrpc service registration
	// with proto-generated code
	_ = server
	_ = svc
}
