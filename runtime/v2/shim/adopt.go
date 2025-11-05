package shim

import (
	"context"

	"github.com/containerd/containerd/errdefs"
)

// AdoptRequest carries minimal information to bind a container context
// onto a prewarmed shim instance. Fields can be extended later (mounts/IO/options).
type AdoptRequest struct {
	ID        string
	Bundle    string
	Namespace string
}

// AdoptContainer is a placeholder client stub that will be replaced by
// a real ttrpc invocation once the task service protobuf adds the RPC.
// For now, it returns Unimplemented so the caller (manager.go) can
// cleanly fallback to the legacy startup path.
func AdoptContainer(ctx context.Context, conn interface{}, req *AdoptRequest) error {
	return errdefs.ErrNotImplemented
}
