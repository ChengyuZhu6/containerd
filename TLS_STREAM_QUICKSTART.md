# TLS Stream 快速开始指南

## 完成后续步骤

### 步骤 1: 安装 Protobuf 工具

```bash
# macOS
brew install protobuf

# 或者使用 Go 安装 protobuild
go install github.com/containerd/protobuild@latest
```

### 步骤 2: 生成 Protobuf 代码

```bash
cd /Users/hudsonzhu/workspace/go/src/github.com/containerd/containerd
make protos
```

### 步骤 3: 验证编译

```bash
go build ./core/transfer/registry/...
```

### 步骤 4: 更新 ctr push 命令

编辑 `cmd/ctr/commands/images/push.go`，添加以下内容：

```go
// 在 flags 中添加
&cli.StringFlag{
    Name:  "tlscacert",
    Usage: "path to TLS CA certificate",
},
&cli.StringFlag{
    Name:  "tlscert",
    Usage: "path to TLS client certificate",
},
&cli.StringFlag{
    Name:  "tlskey",
    Usage: "path to TLS client key",
},

// 在文件开头添加 TLSHelper 实现
type fileTLSHelper struct {
    caCertPath     string
    clientCertPath string
    clientKeyPath  string
}

func (h *fileTLSHelper) GetTLSData(ctx context.Context, host string, dataType transfertypes.TLSRequestType) ([]byte, error) {
    switch dataType {
    case transfertypes.TLSRequestType_CA_CERT:
        if h.caCertPath != "" {
            return os.ReadFile(h.caCertPath)
        }
        return nil, nil
    case transfertypes.TLSRequestType_CLIENT_CERT:
        if h.clientCertPath != "" {
            return os.ReadFile(h.clientCertPath)
        }
        return nil, nil
    case transfertypes.TLSRequestType_CLIENT_KEY:
        if h.clientKeyPath != "" {
            return os.ReadFile(h.clientKeyPath)
        }
        return nil, nil
    }
    return nil, fmt.Errorf("unknown TLS request type: %v", dataType)
}

// 在创建 registry 的地方添加
var regOpts []registry.Opt

// ... 现有的 opts ...

// TLS 配置
if clicontext.String("tlscacert") != "" || clicontext.String("tlscert") != "" {
    helper := &fileTLSHelper{
        caCertPath:     clicontext.String("tlscacert"),
        clientCertPath: clicontext.String("tlscert"),
        clientKeyPath:  clicontext.String("tlskey"),
    }
    regOpts = append(regOpts, registry.WithTLSHelper(helper))
}

if clicontext.Bool("skip-verify") {
    regOpts = append(regOpts, registry.WithSkipVerify(true))
}

// 使用 regOpts 创建 registry
src, err := registry.NewOCIRegistry(ctx, ref, regOpts...)
```

### 步骤 5: 编译 ctr

```bash
make binaries
# 或者只编译 ctr
go build -o bin/ctr ./cmd/ctr
```

### 步骤 6: 测试

```bash
# 准备测试镜像
ctr images pull docker.io/library/alpine:latest
ctr images tag docker.io/library/alpine:latest localhost:5000/test:latest

# 测试 1: 使用自定义 CA
./bin/ctr images push --tlscacert=/path/to/ca.crt localhost:5000/test:latest

# 测试 2: 使用 mTLS
./bin/ctr images push \
  --tlscacert=/path/to/ca.crt \
  --tlscert=/path/to/client.crt \
  --tlskey=/path/to/client.key \
  localhost:5000/test:latest

# 测试 3: 跳过验证
./bin/ctr images push --skip-verify localhost:5000/test:latest
```

## 已实现的文件

### 1. Proto 定义
- ✅ `api/types/transfer/registry.proto`

### 2. 核心实现
- ✅ `core/transfer/registry/registry.go`

### 3. 文档
- ✅ `TLS_CALLBACK_DESIGN.md` - 设计文档
- ✅ `TLS_STREAM_IMPLEMENTATION.md` - 实现文档
- ✅ `TLS_STREAM_SUMMARY.md` - 总结文档
- ✅ `TLS_STREAM_QUICKSTART.md` - 本文档

## 待实现的文件

### 1. 命令行工具
- ⏳ `cmd/ctr/commands/images/push.go` - 需要添加 TLS flags 和 helper

### 2. 测试
- ⏳ `core/transfer/registry/registry_test.go` - 单元测试
- ⏳ `integration/transfer_tls_test.go` - 集成测试

## 关键代码位置

### Proto 定义
```
api/types/transfer/registry.proto:50-98
```

### Registry 实现
```
core/transfer/registry/registry.go:
- Line 18-24: imports (添加了 crypto/tls, crypto/x509)
- Line 48-51: registryOpts 结构体 (添加了 tlsHelper, skipVerify)
- Line 109-127: WithTLSHelper, WithSkipVerify 函数
- Line 276-279: TLSHelper 接口定义
- Line 282-303: OCIRegistry 结构体 (添加了 tlsHelper, skipVerify)
- Line 410-467: MarshalAny 中的 TLS stream 创建
- Line 547-643: UnmarshalAny 中的 TLS stream 处理
- Line 742-771: tlsCallback 实现
```

## 验证清单

- [ ] Protobuf 代码已生成
- [ ] registry 包编译成功
- [ ] ctr push 命令已更新
- [ ] ctr 编译成功
- [ ] 基本 TLS 测试通过
- [ ] mTLS 测试通过
- [ ] skip-verify 测试通过
- [ ] 单元测试已添加
- [ ] 集成测试已添加
- [ ] 文档已更新

## 常见问题

### Q: 编译时提示 undefined: transfertypes.TLSConfig
**A**: 需要先运行 `make protos` 生成 protobuf 代码

### Q: make protos 失败
**A**: 检查是否安装了 protobuf 工具：
```bash
which protoc
# 或
which protobuild
```

### Q: 如何测试 mTLS？
**A**: 参考 `TLS_STREAM_SUMMARY.md` 中的"测试计划"章节

### Q: 证书格式要求？
**A**: 必须是 PEM 格式

## 下一步

1. 完成 protobuf 代码生成
2. 更新 ctr push 命令
3. 添加测试
4. 提交 PR

## 相关文档

- [TLS_CALLBACK_DESIGN.md](./TLS_CALLBACK_DESIGN.md) - 详细设计
- [TLS_STREAM_IMPLEMENTATION.md](./TLS_STREAM_IMPLEMENTATION.md) - 实现细节
- [TLS_STREAM_SUMMARY.md](./TLS_STREAM_SUMMARY.md) - 完整总结
- [TRANSFER_SERVICE_ENHANCEMENT_PLAN.md](./TRANSFER_SERVICE_ENHANCEMENT_PLAN.md) - 整体计划
