package shim

import (
	"context"
	"errors"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// AdoptRequest carries minimal information to bind a container context
// onto a prewarmed shim instance. Fields can be extended later (mounts/IO/options).
type AdoptRequest struct {
	Id        string
	Bundle    string
	Namespace string
}

// AdoptContainer sends AdoptRequest via ttrpc to shim's Task service.
// Returns ErrNotImplemented if the server does not implement the method.
func AdoptContainer(ctx context.Context, conn interface{}, req *AdoptRequest) error {
	cli, ok := conn.(*ttrpc.Client)
	if !ok || cli == nil {
		return errdefs.ErrNotImplemented
	}
	// 建立 ttrpc 通用调用，使用空请求，返回 emptypb.Empty
	var resp emptypb.Empty
	if err := cli.Call(ctx, "containerd.task.v2.Task", "AdoptContainer", &emptypb.Empty{}, &resp); err != nil {
		var ttErr *ttrpc.Error
		if errors.As(err, &ttErr) && ttErr.Code() == ttrpc.Unimplemented {
			return errdefs.ErrNotImplemented
		}
		return err
	}
	return nil
}
