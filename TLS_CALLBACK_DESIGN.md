# TLS Callback 设计方案

## 背景

在 `api/types/transfer/registry.proto` 文件中发现了一段重要的 TODO 注释：

```protobuf
message RegistryResolver {
    string auth_stream = 1;
    map<string, string> headers = 2;
    string host_dir = 3;
    string default_scheme = 4;
    // Force skip verify
    // CA callback? Client TLS callback?
    
    HTTPDebug http_debug = 5;
    string logs_stream = 6;
}
```

这段注释由 **Derek McGowan** 在 2022 年 5 月 23 日创建 Transfer API 时添加（commit `f61ed7e943`），表明他当时就考虑过要支持 TLS 配置，并且倾向于使用 **callback 机制**。

## 为什么使用 Callback？

### 设计理念对比

| 方案 | 实现方式 | 优点 | 缺点 | 适用场景 |
|------|---------|------|------|---------|
| **直接传递** | 在 protobuf 中传递证书内容 | • 实现简单<br>• 无需额外通信 | • 证书可能很大（几 KB）<br>• 敏感数据在 protobuf 中传输<br>• 无法动态更新 | 小证书、测试环境 |
| **文件路径** | 传递证书文件路径 | • 节省传输带宽<br>• 实现简单 | • daemon 需要访问客户端文件系统<br>• 跨主机场景不可用<br>• 权限问题 | 本地开发 |
| **Callback Stream** ✅ | 通过双向流按需获取 | • **安全**（按需获取，不预加载）<br>• **灵活**（支持动态更新）<br>• **一致**（与 auth_stream 设计一致）<br>• **跨平台**（不依赖文件系统） | • 实现复杂度较高<br>• 需要维护 stream 生命周期 | **生产环境推荐** |

### 与 auth_stream 的相似性

Transfer service 已经成功使用了 callback 机制来处理认证：

```
auth_stream:  Client ←→ Daemon (动态获取认证凭据)
tls_stream:   Client ←→ Daemon (动态获取 TLS 证书)  ← 新增
```

两者的设计模式完全一致：

1. **客户端**创建 stream 并启动 goroutine 监听请求
2. **Daemon** 在需要时通过 stream 发送请求
3. **客户端**处理请求并返回数据
4. **Daemon** 使用返回的数据完成操作

## 详细设计

### 1. Protobuf 定义

```protobuf
// api/types/transfer/registry.proto

message RegistryResolver {
    string auth_stream = 1;
    map<string, string> headers = 2;
    string host_dir = 3;
    string default_scheme = 4;
    HTTPDebug http_debug = 5;
    string logs_stream = 6;
    
    // 替换原有的 TODO 注释
    TLSConfig tls_config = 7;
}

// TLS 配置支持两种模式
message TLSConfig {
    // 模式 1: 简单配置（直接传递）
    bool skip_verify = 1;
    bytes ca_cert = 2;      // CA 证书内容 (PEM)
    bytes client_cert = 3;  // 客户端证书 (PEM)
    bytes client_key = 4;   // 客户端密钥 (PEM)
    
    // 模式 2: Callback stream（推荐）
    string tls_stream = 5;  // TLS callback stream ID
}

// TLS 请求消息 (daemon -> client)
message TLSRequest {
    string host = 1;           // registry 主机名
    TLSRequestType type = 2;   // 请求的证书类型
}

enum TLSRequestType {
    CA_CERT = 0;       // 请求 CA 证书
    CLIENT_CERT = 1;   // 请求客户端证书
    CLIENT_KEY = 2;    // 请求客户端密钥
}

// TLS 响应消息 (client -> daemon)
message TLSResponse {
    bytes data = 1;    // PEM 格式的证书/密钥内容
    string error = 2;  // 错误信息（如果有）
}
```

### 2. 工作流程

#### 场景：推送镜像到需要客户端证书的私有 registry

```
┌─────────────┐                                    ┌──────────────────┐
│   Client    │                                    │  containerd      │
│   (ctr)     │                                    │   daemon         │
└──────┬──────┘                                    └────────┬─────────┘
       │                                                    │
       │ 1. Transfer(imageStore, registry)                 │
       │   registry.WithTLSClientConfig("cert.pem", "key.pem")
       │───────────────────────────────────────────────────>│
       │                                                    │
       │ 2. MarshalAny() - 创建 tls_stream                  │
       │    stream_id = "tls-123456789-abc"                │
       │<───────────────────────────────────────────────────│
       │                                                    │
       │ 3. 客户端启动 goroutine 监听 tls_stream             │
       │    for { req := stream.Recv() ... }               │
       │                                                    │
       │                                                    │ 4. Daemon 开始推送
       │                                                    │    需要建立 TLS 连接
       │                                                    │
       │ 5. TLSRequest{type: CA_CERT}                      │
       │<───────────────────────────────────────────────────│
       │                                                    │
       │ 6. 读取 CA 证书文件                                 │
       │    data := os.ReadFile("ca.pem")                  │
       │                                                    │
       │ 7. TLSResponse{data: <PEM content>}               │
       │───────────────────────────────────────────────────>│
       │                                                    │
       │                                                    │ 8. 解析 CA 证书
       │                                                    │    配置 TLS
       │                                                    │
       │ 9. TLSRequest{type: CLIENT_CERT}                  │
       │<───────────────────────────────────────────────────│
       │                                                    │
       │ 10. 读取客户端证书                                  │
       │     data := os.ReadFile("cert.pem")               │
       │                                                    │
       │ 11. TLSResponse{data: <PEM content>}              │
       │───────────────────────────────────────────────────>│
       │                                                    │
       │ 12. TLSRequest{type: CLIENT_KEY}                  │
       │<───────────────────────────────────────────────────│
       │                                                    │
       │ 13. 读取客户端密钥                                  │
       │     data := os.ReadFile("key.pem")                │
       │                                                    │
       │ 14. TLSResponse{data: <PEM content>}              │
       │───────────────────────────────────────────────────>│
       │                                                    │
       │                                                    │ 15. 配置完整 TLS
       │                                                    │     建立安全连接
       │                                                    │     推送镜像
       │                                                    │
       │ 16. Progress updates...                           │
       │<───────────────────────────────────────────────────│
       │                                                    │
       │ 17. Transfer complete                             │
       │<───────────────────────────────────────────────────│
```

### 3. 核心代码实现

#### 客户端侧 - 创建 TLS Stream

```go
// core/transfer/registry/registry.go

type OCIRegistry struct {
    reference string
    headers   http.Header
    creds     CredentialHelper
    resolver  remotes.Resolver
    
    // TLS 配置
    tlsConfig *TLSConfig  // 新增
    
    // ... 其他字段
}

type TLSConfig struct {
    SkipVerify bool
    CACert     string  // CA 证书文件路径
    ClientCert string  // 客户端证书文件路径
    ClientKey  string  // 客户端密钥文件路径
}

func (r *OCIRegistry) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
    res := &transfertypes.RegistryResolver{}
    
    // ... 现有的 auth_stream 代码 ...
    
    // TLS callback stream
    if r.tlsConfig != nil && (r.tlsConfig.CACert != "" || r.tlsConfig.ClientCert != "") {
        sid := tstreaming.GenerateID("tls")
        stream, err := sm.Create(ctx, sid)
        if err != nil {
            return nil, err
        }
        
        // 启动 TLS callback handler
        go r.handleTLSCallback(ctx, stream)
        
        res.TlsConfig = &transfertypes.TLSConfig{
            SkipVerify: r.tlsConfig.SkipVerify,
            TlsStream:  sid,
        }
    } else if r.tlsConfig != nil && r.tlsConfig.SkipVerify {
        // 仅 skip verify
        res.TlsConfig = &transfertypes.TLSConfig{
            SkipVerify: true,
        }
    }
    
    // ... 返回序列化结果
}

func (r *OCIRegistry) handleTLSCallback(ctx context.Context, stream streaming.Stream) {
    defer stream.Close()
    
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }
        
        // 接收 TLS 请求
        req, err := stream.Recv()
        if err != nil {
            if !errors.Is(err, io.EOF) {
                log.G(ctx).WithError(err).Error("TLS stream recv failed")
            }
            return
        }
        
        var tlsReq transfertypes.TLSRequest
        if err := typeurl.UnmarshalTo(req, &tlsReq); err != nil {
            log.G(ctx).WithError(err).Error("failed to unmarshal TLS request")
            continue
        }
        
        // 根据请求类型读取证书
        var data []byte
        var readErr error
        
        switch tlsReq.Type {
        case transfertypes.TLSRequestType_CA_CERT:
            if r.tlsConfig.CACert != "" {
                data, readErr = os.ReadFile(r.tlsConfig.CACert)
                log.G(ctx).WithField("file", r.tlsConfig.CACert).Debug("reading CA cert")
            }
        case transfertypes.TLSRequestType_CLIENT_CERT:
            if r.tlsConfig.ClientCert != "" {
                data, readErr = os.ReadFile(r.tlsConfig.ClientCert)
                log.G(ctx).WithField("file", r.tlsConfig.ClientCert).Debug("reading client cert")
            }
        case transfertypes.TLSRequestType_CLIENT_KEY:
            if r.tlsConfig.ClientKey != "" {
                data, readErr = os.ReadFile(r.tlsConfig.ClientKey)
                log.G(ctx).WithField("file", r.tlsConfig.ClientKey).Debug("reading client key")
            }
        }
        
        // 构造响应
        resp := &transfertypes.TLSResponse{}
        if readErr != nil {
            resp.Error = readErr.Error()
            log.G(ctx).WithError(readErr).Error("failed to read TLS file")
        } else {
            resp.Data = data
        }
        
        // 发送响应
        a, err := typeurl.MarshalAny(resp)
        if err != nil {
            log.G(ctx).WithError(err).Error("failed to marshal TLS response")
            continue
        }
        
        if err := stream.Send(a); err != nil {
            if !errors.Is(err, io.EOF) {
                log.G(ctx).WithError(err).Error("failed to send TLS response")
            }
            return
        }
    }
}
```

#### Daemon 侧 - 使用 TLS Stream

```go
// core/transfer/registry/registry.go

func (r *OCIRegistry) UnmarshalAny(ctx context.Context, sm streaming.StreamGetter, a typeurl.Any) error {
    var s transfertypes.OCIRegistry
    if err := typeurl.UnmarshalTo(a, &s); err != nil {
        return err
    }
    
    hostOptions := config.HostOptions{}
    
    // ... 现有的 auth_stream 代码 ...
    
    // TLS 配置
    if s.Resolver != nil && s.Resolver.TlsConfig != nil {
        tlsConf := s.Resolver.TlsConfig
        
        // 保存原有的 UpdateClient
        originalUpdateClient := hostOptions.UpdateClient
        
        hostOptions.UpdateClient = func(client *http.Client) error {
            // 调用原有的 UpdateClient（如果有）
            if originalUpdateClient != nil {
                if err := originalUpdateClient(client); err != nil {
                    return err
                }
            }
            
            // 配置 TLS
            transport, ok := client.Transport.(*http.Transport)
            if !ok {
                transport = &http.Transport{}
                client.Transport = transport
            }
            
            if transport.TLSClientConfig == nil {
                transport.TLSClientConfig = &tls.Config{}
            }
            
            // Skip verify
            if tlsConf.SkipVerify {
                transport.TLSClientConfig.InsecureSkipVerify = true
            }
            
            // 直接传递的证书（简单模式）
            if len(tlsConf.CaCert) > 0 {
                caCertPool := x509.NewCertPool()
                if !caCertPool.AppendCertsFromPEM(tlsConf.CaCert) {
                    return fmt.Errorf("failed to parse CA certificate")
                }
                transport.TLSClientConfig.RootCAs = caCertPool
            }
            
            if len(tlsConf.ClientCert) > 0 && len(tlsConf.ClientKey) > 0 {
                cert, err := tls.X509KeyPair(tlsConf.ClientCert, tlsConf.ClientKey)
                if err != nil {
                    return fmt.Errorf("failed to load client certificate: %w", err)
                }
                transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
            }
            
            // TLS callback stream（推荐模式）
            if sid := tlsConf.TlsStream; sid != "" {
                stream, err := sm.Get(ctx, sid)
                if err != nil {
                    return fmt.Errorf("failed to get TLS stream: %w", err)
                }
                
                tlsCallback := &tlsCallback{stream: stream}
                
                // 获取 CA 证书
                caCertData, err := tlsCallback.GetCert(ctx, s.Reference, transfertypes.TLSRequestType_CA_CERT)
                if err == nil && len(caCertData) > 0 {
                    caCertPool := x509.NewCertPool()
                    if !caCertPool.AppendCertsFromPEM(caCertData) {
                        return fmt.Errorf("failed to parse CA certificate from stream")
                    }
                    transport.TLSClientConfig.RootCAs = caCertPool
                    log.G(ctx).Debug("loaded CA cert from TLS stream")
                }
                
                // 获取客户端证书
                clientCertData, err := tlsCallback.GetCert(ctx, s.Reference, transfertypes.TLSRequestType_CLIENT_CERT)
                if err == nil && len(clientCertData) > 0 {
                    clientKeyData, err := tlsCallback.GetCert(ctx, s.Reference, transfertypes.TLSRequestType_CLIENT_KEY)
                    if err == nil && len(clientKeyData) > 0 {
                        cert, err := tls.X509KeyPair(clientCertData, clientKeyData)
                        if err != nil {
                            return fmt.Errorf("failed to load client certificate from stream: %w", err)
                        }
                        transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
                        log.G(ctx).Debug("loaded client cert from TLS stream")
                    }
                }
            }
            
            return nil
        }
    }
    
    // ... 创建 resolver
}

// TLS callback 实现
type tlsCallback struct {
    sync.Mutex
    stream streaming.Stream
}

func (tc *tlsCallback) GetCert(ctx context.Context, host string, certType transfertypes.TLSRequestType) ([]byte, error) {
    tc.Lock()
    defer tc.Unlock()
    
    req := &transfertypes.TLSRequest{
        Host: host,
        Type: certType,
    }
    
    anyType, err := typeurl.MarshalAny(req)
    if err != nil {
        return nil, fmt.Errorf("failed to marshal TLS request: %w", err)
    }
    
    if err := tc.stream.Send(anyType); err != nil {
        return nil, fmt.Errorf("failed to send TLS request: %w", err)
    }
    
    resp, err := tc.stream.Recv()
    if err != nil {
        return nil, fmt.Errorf("failed to receive TLS response: %w", err)
    }
    
    var tlsResp transfertypes.TLSResponse
    if err := typeurl.UnmarshalTo(resp, &tlsResp); err != nil {
        return nil, fmt.Errorf("failed to unmarshal TLS response: %w", err)
    }
    
    if tlsResp.Error != "" {
        return nil, fmt.Errorf("TLS callback error: %s", tlsResp.Error)
    }
    
    return tlsResp.Data, nil
}
```

### 4. CLI 使用示例

```bash
# 使用 TLS callback stream（推荐）
ctr images push \
  --tlscacert /path/to/ca.pem \
  --tlscert /path/to/client-cert.pem \
  --tlskey /path/to/client-key.pem \
  registry.example.com/myimage:latest

# 跳过 TLS 验证（仅测试环境）
ctr images push \
  --skip-verify \
  registry.example.com/myimage:latest

# 组合使用
ctr images push \
  --tlscacert /path/to/ca.pem \
  --skip-verify \
  registry.example.com/myimage:latest
```

## 优势总结

### 1. 安全性
- ✅ 证书内容不会存储在 daemon 侧
- ✅ 按需获取，减少敏感数据暴露时间
- ✅ 通过加密的 gRPC 连接传输

### 2. 灵活性
- ✅ 支持动态更新证书（如证书轮换）
- ✅ 支持多种 TLS 配置场景
- ✅ 客户端完全控制证书来源

### 3. 一致性
- ✅ 与 `auth_stream` 设计模式完全一致
- ✅ 遵循 Transfer Service 的设计理念
- ✅ 符合原作者的设计意图

### 4. 可扩展性
- ✅ 未来可以支持更多 TLS 配置选项
- ✅ 可以添加证书验证回调
- ✅ 可以支持动态 SNI 配置

## 与现有机制的对比

| 特性 | auth_stream | tls_stream | logs_stream | progress_stream |
|------|-------------|------------|-------------|-----------------|
| **方向** | 双向 | 双向 | 单向 | 单向 |
| **用途** | 认证凭据 | TLS 证书 | HTTP 日志 | 进度更新 |
| **请求/响应** | ✅ | ✅ | ❌ | ❌ |
| **安全敏感** | ✅ | ✅ | ❌ | ❌ |
| **按需获取** | ✅ | ✅ | ❌ | ❌ |
| **实现复杂度** | 中 | 中 | 低 | 低 |

## 实施建议

### Phase 1: 基础实现
1. 更新 `registry.proto` 添加 TLS 相关消息定义
2. 实现简单模式（直接传递证书内容）
3. 添加 `--skip-verify` 支持

### Phase 2: Callback 实现
1. 实现 TLS callback stream 机制
2. 添加客户端侧的 TLS handler
3. 添加 daemon 侧的 TLS callback

### Phase 3: CLI 集成
1. 添加 `--tlscacert`, `--tlscert`, `--tlskey` flags
2. 更新 `push.go` 移除 TLS 相关的限制
3. 添加使用示例和文档

### Phase 4: 测试与优化
1. 添加单元测试
2. 添加集成测试（使用自签名证书的 registry）
3. 性能测试和优化

## 参考资料

- [Transfer API 初始提交](https://github.com/containerd/containerd/commit/f61ed7e943645ab346cfef42f6962c6b063845e0)
- [auth_stream 实现](https://github.com/containerd/containerd/blob/main/core/transfer/registry/registry.go)
- [Go TLS 配置文档](https://pkg.go.dev/crypto/tls)
- [Docker Registry TLS 配置](https://docs.docker.com/registry/insecure/)
