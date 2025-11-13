# TLS Stream Implementation for Transfer Service

## 概述

本文档描述了在 containerd transfer service 中实现的 TLS stream 功能。该功能允许客户端通过 callback stream 机制动态提供 TLS 证书和密钥，支持完整的 mTLS（mutual TLS）场景。

## 设计原则

遵循现有的 `auth_stream` 设计模式：
- **纯 Callback Stream 模式**：不在 protobuf 中直接传递证书/密钥数据
- **动态获取**：按需从客户端获取 TLS 数据
- **安全性**：敏感数据不在 daemon 侧存储
- **一致性**：与现有 auth_stream 机制保持一致

## 实现的文件

### 1. Proto 定义 (`api/types/transfer/registry.proto`)

添加了以下消息类型：

```protobuf
// TLS 配置
message TLSConfig {
    bool skip_verify = 1;        // 跳过 TLS 验证
    string tls_stream = 2;       // TLS callback stream ID
}

// TLS 请求类型
enum TLSRequestType {
    CA_CERT = 0;      // CA 证书（用于验证服务器）
    CLIENT_CERT = 1;  // 客户端证书（用于 mTLS）
    CLIENT_KEY = 2;   // 客户端私钥（用于 mTLS）
}

// TLS 请求
message TLSRequest {
    string host = 1;              // 目标主机
    TLSRequestType type = 2;      // 请求的数据类型
}

// TLS 响应
message TLSResponse {
    bytes data = 1;    // PEM 格式的证书或密钥
    string error = 2;  // 错误信息（如果有）
}
```

在 `RegistryResolver` 中添加：
```protobuf
TLSConfig tls = 7;
```

### 2. Registry 实现 (`core/transfer/registry/registry.go`)

#### 新增接口

```go
// TLSHelper 提供 TLS 证书和密钥的动态获取
type TLSHelper interface {
    GetTLSData(ctx context.Context, host string, dataType transfertypes.TLSRequestType) ([]byte, error)
}
```

#### 新增选项函数

```go
// WithTLSHelper 配置 TLS helper
func WithTLSHelper(helper TLSHelper) Opt

// WithSkipVerify 禁用 TLS 证书验证
func WithSkipVerify(skip bool) Opt
```

#### OCIRegistry 结构体扩展

添加字段：
```go
tlsHelper  TLSHelper
skipVerify bool
```

#### MarshalAny 方法扩展

在客户端侧创建 TLS stream：
1. 生成 stream ID: `tstreaming.GenerateID("tls")`
2. 创建 stream: `sm.Create(ctx, sid)`
3. 启动 goroutine 监听 TLS 请求
4. 接收 `TLSRequest`，调用 `tlsHelper.GetTLSData()`
5. 发送 `TLSResponse`

#### UnmarshalAny 方法扩展

在 daemon 侧处理 TLS stream：
1. 获取 stream: `sm.Get(ctx, sid)`
2. 创建 `tlsCallback` 实现 `TLSHelper` 接口
3. 配置 `tls.Config`:
   - `GetClientCertificate`: 动态获取客户端证书和密钥
   - `VerifyPeerCertificate`: 使用自定义 CA 验证服务器证书
   - `InsecureSkipVerify`: 根据 `skip_verify` 设置

#### tlsCallback 实现

```go
type tlsCallback struct {
    sync.Mutex
    stream streaming.Stream
}

func (tc *tlsCallback) GetTLSData(ctx context.Context, host string, dataType transfertypes.TLSRequestType) ([]byte, error) {
    // 1. 发送 TLSRequest
    // 2. 接收 TLSResponse
    // 3. 返回数据或错误
}
```

## 使用示例

### 客户端侧（ctr push）

```go
// 实现 TLSHelper
type myTLSHelper struct {
    caCertPath     string
    clientCertPath string
    clientKeyPath  string
}

func (h *myTLSHelper) GetTLSData(ctx context.Context, host string, dataType transfertypes.TLSRequestType) ([]byte, error) {
    switch dataType {
    case transfertypes.TLSRequestType_CA_CERT:
        return os.ReadFile(h.caCertPath)
    case transfertypes.TLSRequestType_CLIENT_CERT:
        return os.ReadFile(h.clientCertPath)
    case transfertypes.TLSRequestType_CLIENT_KEY:
        return os.ReadFile(h.clientKeyPath)
    }
    return nil, fmt.Errorf("unknown TLS request type")
}

// 创建 registry
helper := &myTLSHelper{
    caCertPath:     "/path/to/ca.crt",
    clientCertPath: "/path/to/client.crt",
    clientKeyPath:  "/path/to/client.key",
}

registry, err := registry.NewOCIRegistry(ctx, ref,
    registry.WithTLSHelper(helper),
    registry.WithSkipVerify(false),
)
```

### Daemon 侧

Daemon 侧自动处理：
1. 从 protobuf 中提取 TLS stream ID
2. 获取 stream 并创建 tlsCallback
3. 在 TLS 握手时通过 callback 获取证书/密钥
4. 验证服务器证书（如果提供了 CA）

## 工作流程

### Push 操作流程

```
Client (ctr)                    Daemon (containerd)              Registry
    |                                  |                              |
    | 1. Transfer(OCIRegistry)         |                              |
    |----------------------------------->                              |
    |    with tls_stream ID            |                              |
    |                                  |                              |
    |                                  | 2. Connect to registry       |
    |                                  |----------------------------->|
    |                                  |                              |
    |                                  | 3. TLS Handshake             |
    |                                  |<---------------------------->|
    |                                  |                              |
    | 4. TLSRequest(CA_CERT)           |                              |
    |<----------------------------------|                              |
    |                                  |                              |
    | 5. TLSResponse(ca.pem)           |                              |
    |----------------------------------->                              |
    |                                  |                              |
    | 6. TLSRequest(CLIENT_CERT)       |                              |
    |<----------------------------------|                              |
    |                                  |                              |
    | 7. TLSResponse(client.crt)       |                              |
    |----------------------------------->                              |
    |                                  |                              |
    | 8. TLSRequest(CLIENT_KEY)        |                              |
    |<----------------------------------|                              |
    |                                  |                              |
    | 9. TLSResponse(client.key)       |                              |
    |----------------------------------->                              |
    |                                  |                              |
    |                                  | 10. TLS established          |
    |                                  |<---------------------------->|
    |                                  |                              |
    |                                  | 11. Push image               |
    |                                  |----------------------------->|
```

## 支持的场景

1. **跳过 TLS 验证**
   ```go
   registry.WithSkipVerify(true)
   ```

2. **自定义 CA 证书**
   ```go
   registry.WithTLSHelper(helper) // 提供 CA_CERT
   ```

3. **客户端认证 (mTLS)**
   ```go
   registry.WithTLSHelper(helper) // 提供 CLIENT_CERT + CLIENT_KEY
   ```

4. **完整 mTLS + 自定义 CA**
   ```go
   registry.WithTLSHelper(helper) // 提供所有三种类型
   ```

## 与现有功能的对比

| 功能 | auth_stream | tls_stream |
|------|-------------|------------|
| 用途 | 认证凭据 | TLS 证书/密钥 |
| 请求类型 | AuthRequest | TLSRequest |
| 响应类型 | AuthResponse | TLSResponse |
| 数据类型 | username/password/token | PEM 证书/密钥 |
| 触发时机 | HTTP 401/407 | TLS 握手 |
| 实现模式 | 纯 stream | 纯 stream |

## 下一步工作

1. **生成 Protobuf 代码**
   ```bash
   make protos
   ```

2. **更新 ctr push 命令**
   - 添加 `--tlscacert`, `--tlscert`, `--tlskey` flags
   - 实现 TLSHelper 接口
   - 集成到 transfer service

3. **测试**
   - 单元测试
   - 集成测试（与真实 registry 交互）
   - mTLS 场景测试

4. **文档**
   - 更新用户文档
   - 添加示例配置

## 注意事项

1. **证书格式**：所有证书和密钥必须是 PEM 格式
2. **错误处理**：TLSResponse 中的 error 字段用于传递错误信息
3. **性能**：证书在每次 TLS 握手时动态获取，可能有轻微性能影响
4. **安全性**：证书/密钥通过 stream 传输，不在 daemon 侧持久化

## 参考

- 原始设计讨论：`TLS_CALLBACK_DESIGN.md`
- Proto 文件：`api/types/transfer/registry.proto`
- 实现文件：`core/transfer/registry/registry.go`
- Auth stream 实现：参考 `credCallback` 结构体
