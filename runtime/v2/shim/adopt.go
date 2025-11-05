package shim

import (
	"context"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/ttrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	// 通过 ttrpc metadata 传递容器上下文字段，避免生成 proto
	mdMD := ttrpc.MD{
		"adopt.id":        []string{req.Id},
		"adopt.bundle":    []string{req.Bundle},
		"adopt.namespace": []string{req.Namespace},
	}
	ctx = ttrpc.WithMetadata(ctx, mdMD)

	// 建立 ttrpc 调用，使用空请求，返回 emptypb.Empty
	var resp emptypb.Empty
	if err := cli.Call(ctx, "containerd.task.v2.Task", "AdoptContainer", &emptypb.Empty{}, &resp); err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
			return errdefs.ErrNotImplemented
		}
		return err
	}
	return nil
}
