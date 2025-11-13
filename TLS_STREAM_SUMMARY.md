# TLS Stream 功能实现总结

## 已完成的工作

### 1. Proto 定义更新 ✅

文件：`api/types/transfer/registry.proto`

- 添加了 `TLSConfig` 消息类型
- 添加了 `TLSRequestType` 枚举（CA_CERT, CLIENT_CERT, CLIENT_KEY）
- 添加了 `TLSRequest` 和 `TLSResponse` 消息类型
- 在 `RegistryResolver` 中添加了 `TLSConfig tls = 7` 字段
- 移除了原有的 TODO 注释 `// Force skip verify // CA callback? Client TLS callback?`

### 2. Registry 核心实现 ✅

文件：`core/transfer/registry/registry.go`

#### 新增接口和类型
- `TLSHelper` 接口：定义 TLS 数据获取方法
- `tlsCallback` 结构体：实现 daemon 侧的 TLS callback

#### 新增配置选项
- `WithTLSHelper(helper TLSHelper)`: 配置 TLS helper
- `WithSkipVerify(skip bool)`: 配置是否跳过 TLS 验证

#### 扩展 OCIRegistry 结构体
- 添加 `tlsHelper TLSHelper` 字段
- 添加 `skipVerify bool` 字段

#### 客户端侧实现（MarshalAny）
- 创建 TLS stream
- 启动 goroutine 监听 TLS 请求
- 处理 `TLSRequest` 并返回 `TLSResponse`

#### Daemon 侧实现（UnmarshalAny）
- 获取 TLS stream
- 创建 `tlsCallback` 实现
- 配置 `tls.Config`:
  - `GetClientCertificate`: 动态获取客户端证书
  - `VerifyPeerCertificate`: 使用自定义 CA 验证
  - `InsecureSkipVerify`: 根据配置设置

#### 导入更新
- 添加 `crypto/tls`
- 添加 `crypto/x509`

### 3. 文档 ✅

- `TLS_STREAM_IMPLEMENTATION.md`: 完整的实现文档
- `TLS_STREAM_SUMMARY.md`: 本总结文档

## 需要完成的后续工作

### 1. 生成 Protobuf 代码 ⚠️

**问题**：当前环境缺少 protobuf 编译工具

**解决方案**：
```bash
# 安装 protobuild（如果需要）
go install github.com/containerd/protobuild@latest

# 或者安装 protoc
brew install protobuf  # macOS
# 或
apt-get install protobuf-compiler  # Linux

# 生成代码
make protos
```

**生成的文件**：
- `api/types/transfer/registry.pb.go`
- 其他相关的 protobuf 生成文件

### 2. 修复编译错误 ⚠️

当前编译错误（需要生成 protobuf 代码后解决）：
```
undefined: transfertypes.TLSRequestType_CLIENT_CERT
undefined: transfertypes.TLSRequestType_CLIENT_KEY
undefined: transfertypes.TLSRequestType_CA_CERT
undefined: transfertypes.TLSConfig
undefined: transfertypes.TLSRequest
undefined: transfertypes.TLSResponse
```

### 3. 更新 ctr push 命令 📝

文件：`cmd/ctr/commands/images/push.go`

需要添加：
```go
// 添加 flags
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

// 实现 TLSHelper
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
    case transfertypes.TLSRequestType_CLIENT_CERT:
        if h.clientCertPath != "" {
            return os.ReadFile(h.clientCertPath)
        }
    case transfertypes.TLSRequestType_CLIENT_KEY:
        if h.clientKeyPath != "" {
            return os.ReadFile(h.clientKeyPath)
        }
    }
    return nil, fmt.Errorf("TLS data not configured for type %v", dataType)
}

// 在创建 registry 时使用
var opts []registry.Opt
if clicontext.String("tlscacert") != "" || clicontext.String("tlscert") != "" {
    helper := &fileTLSHelper{
        caCertPath:     clicontext.String("tlscacert"),
        clientCertPath: clicontext.String("tlscert"),
        clientKeyPath:  clicontext.String("tlskey"),
    }
    opts = append(opts, registry.WithTLSHelper(helper))
}
if clicontext.Bool("skip-verify") {
    opts = append(opts, registry.WithSkipVerify(true))
}
```

### 4. 添加测试 📝

#### 单元测试
文件：`core/transfer/registry/registry_test.go`

测试内容：
- `tlsCallback.GetTLSData()` 方法
- TLS stream 的创建和通信
- 错误处理

#### 集成测试
文件：`integration/transfer_tls_test.go`

测试场景：
- 使用自定义 CA 的 push
- 使用 mTLS 的 push
- skip-verify 的 push
- 错误场景（证书不匹配等）

### 5. 更新文档 📝

需要更新的文档：
- `docs/transfer.md`: 添加 TLS 配置说明
- `docs/ctr/push.md`: 添加 TLS flags 说明
- `RELEASES.md`: 添加新功能说明

### 6. 代码审查检查项 📝

- [ ] 错误处理是否完善
- [ ] 日志记录是否充分
- [ ] 是否有资源泄漏（goroutine, stream）
- [ ] 是否需要添加超时控制
- [ ] 是否需要添加重试逻辑
- [ ] 代码风格是否符合项目规范

## 测试计划

### 环境准备

1. **设置测试 Registry**
   ```bash
   # 使用 Docker 启动支持 mTLS 的 registry
   docker run -d -p 5000:5000 \
     -v /path/to/certs:/certs \
     -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/server.crt \
     -e REGISTRY_HTTP_TLS_KEY=/certs/server.key \
     -e REGISTRY_HTTP_TLS_CLIENTCAS_0=/certs/ca.crt \
     registry:2
   ```

2. **生成测试证书**
   ```bash
   # 生成 CA
   openssl genrsa -out ca.key 4096
   openssl req -new -x509 -days 365 -key ca.key -out ca.crt

   # 生成服务器证书
   openssl genrsa -out server.key 4096
   openssl req -new -key server.key -out server.csr
   openssl x509 -req -days 365 -in server.csr -CA ca.crt -CAkey ca.key -out server.crt

   # 生成客户端证书
   openssl genrsa -out client.key 4096
   openssl req -new -key client.key -out client.csr
   openssl x509 -req -days 365 -in client.csr -CA ca.crt -CAkey ca.key -out client.crt
   ```

### 测试用例

1. **基本 TLS (HTTPS)**
   ```bash
   ctr images push --tlscacert=ca.crt localhost:5000/test:latest
   ```

2. **mTLS**
   ```bash
   ctr images push \
     --tlscacert=ca.crt \
     --tlscert=client.crt \
     --tlskey=client.key \
     localhost:5000/test:latest
   ```

3. **Skip Verify**
   ```bash
   ctr images push --skip-verify localhost:5000/test:latest
   ```

4. **错误场景**
   - 证书不匹配
   - 证书过期
   - 缺少客户端证书（当 registry 要求时）

## 性能考虑

1. **证书缓存**：当前实现每次 TLS 握手都会请求证书，可以考虑在客户端侧缓存
2. **Stream 开销**：每次请求都需要通过 stream 通信，有一定延迟
3. **并发连接**：多个并发连接会导致多次证书请求

## 安全考虑

1. **证书传输**：证书通过 stream 传输，确保 stream 本身的安全性
2. **密钥保护**：私钥在客户端侧读取，不在 daemon 侧存储
3. **错误信息**：避免在错误信息中泄露敏感信息

## 兼容性

- **向后兼容**：不使用 TLS stream 的现有代码不受影响
- **可选功能**：TLS stream 是可选的，不影响基本功能
- **Proto 版本**：新增字段使用新的 field number (7)，不影响旧版本

## 参考资料

- [Go TLS 文档](https://pkg.go.dev/crypto/tls)
- [X.509 证书](https://pkg.go.dev/crypto/x509)
- [containerd Transfer Service](https://github.com/containerd/containerd/blob/main/docs/transfer.md)
- [gRPC Streaming](https://grpc.io/docs/what-is-grpc/core-concepts/#server-streaming-rpc)

## 联系方式

如有问题，请参考：
- `TLS_CALLBACK_DESIGN.md` - 详细设计文档
- `TLS_STREAM_IMPLEMENTATION.md` - 实现文档
- `TRANSFER_SERVICE_ENHANCEMENT_PLAN.md` - 整体增强计划
