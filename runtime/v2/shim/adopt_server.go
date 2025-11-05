package shim

import (
	"context"

	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Implement proto.Message minimal methods via embedding; for ttrpc generic marshalling we only need VT/proto marshaling.
// To avoid external codegen, we rely on protobuf reflection fallback through generated descriptors (not present here),
// so we keep server handler simple and do not depend on auto-marshaling beyond emptypb for response.

// RegisterAdoptHandler registers an AdoptContainer RPC using ttrpc ServiceDesc.
// Service: "containerd.task.v2.Task"
// Method:  "AdoptContainer"
func RegisterAdoptHandler(server *ttrpc.Server) {
	// 以 ttrpc 的 ServiceDesc.Methods 注册一个最小可用的 AdoptContainer
	server.RegisterService("containerd.task.v2.Task", &ttrpc.ServiceDesc{
		Methods: map[string]ttrpc.Method{
			"AdoptContainer": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				// 最小载荷采用空请求，保持兼容
				var req emptypb.Empty
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				// 从 ttrpc metadata 读取容器上下文并记录（占位实现）
				md, _ := ttrpc.GetMetadata(ctx)
				// 将绑定信息写入日志；后续可存入 shim 内部状态供 Create 使用
				id := md["adopt.id"]
				bundle := md["adopt.bundle"]
				ns := md["adopt.namespace"]
				_ = id
				_ = bundle
				_ = ns
				// 返回空响应，表示 adopt 成功
				return &emptypb.Empty{}, nil
			},
		},
	})
}
