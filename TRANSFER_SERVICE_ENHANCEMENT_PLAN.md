# Transfer Service 功能增强方案

## 背景

目前 transfer service 默认的 registry 不支持以下 flags，这些功能在 `--local` 模式下可用：
- `manifest`, `manifest-type` - 指定特定 manifest digest
- `max-concurrent-uploaded-layers` - 并发上传限制
- `allow-non-distributable-blobs` - 允许推送非分发 blobs
- `skip-verify`, `tlscacert`, `tlscert`, `tlskey` - TLS 配置
- `http-dump`, `http-trace` - HTTP 调试

## 当前状态分析

### ✅ 已支持的功能

1. **HTTP 调试功能** (`http-dump`, `http-trace`)
   - 位置: `core/transfer/registry/registry.go`
   - 实现: `WithHTTPDebug()`, `WithHTTPTrace()`
   - Protobuf: `api/types/transfer/registry.proto` 中的 `HTTPDebug` 枚举

2. **并发上传限制** (`max-concurrent-uploaded-layers`)
   - 位置: `core/transfer/local/transfer.go`
   - 实现: `TransferConfig.MaxConcurrentUploadedLayers`
   - 使用: `ts.limiterU` semaphore

### ⚠️ 需要实现的功能

## 实现方案

### 1. 支持 `manifest` 和 `manifest-type`

**问题**: 当前 `image.Store` 只能通过镜像名称获取，无法直接指定 manifest digest。

**解决方案**:

#### 1.1 扩展 `image.Store` 选项

```go
// core/transfer/image/store.go

type storeOpts struct {
    platforms       []ocispec.Platform
    unpack          []UnpackConfiguration
    digestRef       bool
    allMetadata     bool
    manifest        *ocispec.Descriptor  // 新增: 指定 manifest
}

// WithManifest 指定要推送的 manifest descriptor
func WithManifest(desc ocispec.Descriptor) StoreOpt {
    return func(o *storeOpts) error {
        o.manifest = &desc
        return nil
    }
}
```

#### 1.2 修改 `ImageStore.Get()` 方法

```go
// core/transfer/image/store.go

func (s *ImageStore) Get(ctx context.Context, store images.Store) (images.Image, error) {
    img, err := store.Get(ctx, s.name)
    if err != nil {
        return images.Image{}, err
    }
    
    // 如果指定了 manifest，使用指定的 descriptor
    if s.opts.manifest != nil {
        img.Target = *s.opts.manifest
    }
    
    return img, nil
}
```

#### 1.3 更新 Protobuf 定义

```protobuf
// api/types/transfer/imagestore.proto

message ImageStore {
    string name = 1;
    repeated Platform platforms = 2;
    repeated UnpackConfiguration unpacks = 3;
    bool all_metadata = 4;
    
    // 新增字段
    optional Descriptor manifest = 5;  // 指定的 manifest descriptor
}
```

### 2. 支持 `allow-non-distributable-blobs`

**问题**: 当前 push 流程没有过滤非分发 blobs 的选项。

**解决方案**:

#### 2.1 扩展 `transfer.Config`

```go
// core/transfer/transfer.go

type Config struct {
    Progress ProgressFunc
    
    // 新增: 允许推送非分发 blobs
    AllowNonDistributableBlobs bool
}

// WithAllowNonDistributableBlobs 允许推送非分发 blobs
func WithAllowNonDistributableBlobs(allow bool) Opt {
    return func(c *Config) {
        c.AllowNonDistributableBlobs = allow
    }
}
```

#### 2.2 修改 push 实现

```go
// core/transfer/local/push.go

func (ts *localTransferService) push(ctx context.Context, ig transfer.ImageGetter, p transfer.ImagePusher, tops *transfer.Config) error {
    // ... 现有代码 ...
    
    var wrapper func(images.Handler) images.Handler
    
    // 添加非分发 blob 过滤器
    if !tops.AllowNonDistributableBlobs {
        baseWrapper := wrapper
        wrapper = func(h images.Handler) images.Handler {
            h = remotes.SkipNonDistributableBlobs(h)
            if baseWrapper != nil {
                h = baseWrapper(h)
            }
            return h
        }
    }
    
    // ... 现有代码 ...
}
```

#### 2.3 更新 Protobuf 定义

```protobuf
// api/types/transfer/transfer.proto (需要创建)

message TransferOptions {
    string progress_stream = 1;
    bool allow_non_distributable_blobs = 2;
}
```

### 3. 支持 TLS 配置 (`skip-verify`, `tlscacert`, `tlscert`, `tlskey`)

**问题**: 当前 registry 配置不支持自定义 TLS 设置。

**解决方案**:

#### 3.1 扩展 `registryOpts`

```go
// core/transfer/registry/registry.go

type registryOpts struct {
    headers       http.Header
    creds         CredentialHelper
    hostDir       string
    defaultScheme string
    httpDebug     bool
    httpTrace     bool
    localStream   io.WriteCloser
    
    // 新增 TLS 配置
    tlsConfig     *TLSConfig
}

type TLSConfig struct {
    SkipVerify bool
    CACert     string  // CA 证书路径
    ClientCert string  // 客户端证书路径
    ClientKey  string  // 客户端密钥路径
}

// WithTLSConfig 配置 TLS 选项
func WithTLSConfig(config *TLSConfig) Opt {
    return func(o *registryOpts) error {
        o.tlsConfig = config
        return nil
    }
}

// WithSkipVerify 跳过 TLS 验证
func WithSkipVerify(skip bool) Opt {
    return func(o *registryOpts) error {
        if o.tlsConfig == nil {
            o.tlsConfig = &TLSConfig{}
        }
        o.tlsConfig.SkipVerify = skip
        return nil
    }
}

// WithTLSClientConfig 配置客户端证书
func WithTLSClientConfig(certFile, keyFile string) Opt {
    return func(o *registryOpts) error {
        if o.tlsConfig == nil {
            o.tlsConfig = &TLSConfig{}
        }
        o.tlsConfig.ClientCert = certFile
        o.tlsConfig.ClientKey = keyFile
        return nil
    }
}

// WithTLSCACert 配置 CA 证书
func WithTLSCACert(caFile string) Opt {
    return func(o *registryOpts) error {
        if o.tlsConfig == nil {
            o.tlsConfig = &TLSConfig{}
        }
        o.tlsConfig.CACert = caFile
        return nil
    }
}
```

#### 3.2 修改 `NewOCIRegistry` 实现

```go
// core/transfer/registry/registry.go

func NewOCIRegistry(ctx context.Context, ref string, opts ...Opt) (*OCIRegistry, error) {
    // ... 现有代码 ...
    
    hostOptions.UpdateClient = func(client *http.Client) error {
        // TLS 配置
        if ropts.tlsConfig != nil {
            tlsConfig := &tls.Config{}
            
            if ropts.tlsConfig.SkipVerify {
                tlsConfig.InsecureSkipVerify = true
            }
            
            // 加载 CA 证书
            if ropts.tlsConfig.CACert != "" {
                caCert, err := os.ReadFile(ropts.tlsConfig.CACert)
                if err != nil {
                    return fmt.Errorf("failed to read CA cert: %w", err)
                }
                caCertPool := x509.NewCertPool()
                caCertPool.AppendCertsFromPEM(caCert)
                tlsConfig.RootCAs = caCertPool
            }
            
            // 加载客户端证书
            if ropts.tlsConfig.ClientCert != "" && ropts.tlsConfig.ClientKey != "" {
                cert, err := tls.LoadX509KeyPair(ropts.tlsConfig.ClientCert, ropts.tlsConfig.ClientKey)
                if err != nil {
                    return fmt.Errorf("failed to load client cert: %w", err)
                }
                tlsConfig.Certificates = []tls.Certificate{cert}
            }
            
            // 应用 TLS 配置
            if transport, ok := client.Transport.(*http.Transport); ok {
                transport.TLSClientConfig = tlsConfig
            } else {
                client.Transport = &http.Transport{
                    TLSClientConfig: tlsConfig,
                }
            }
        }
        
        // HTTP 调试
        if ropts.httpDebug {
            httpdbg.DumpRequests(ctx, client, ropts.localStream)
        }
        if ropts.httpTrace {
            httpdbg.DumpTraces(ctx, client)
        }
        return nil
    }
    
    // ... 现有代码 ...
}
```

#### 3.3 更新 Protobuf 定义

**注意**: proto 文件中已有注释 `// Force skip verify // CA callback? Client TLS callback?`，
表明原作者 Derek McGowan 在 2022 年就考虑过这个功能，建议使用 callback 机制。

```protobuf
// api/types/transfer/registry.proto

message RegistryResolver {
    string auth_stream = 1;
    map<string, string> headers = 2;
    string host_dir = 3;
    string default_scheme = 4;
    HTTPDebug http_debug = 5;
    string logs_stream = 6;
    
    // 新增 TLS 配置 (替换原有的 TODO 注释)
    TLSConfig tls_config = 7;
}

// TLS 配置支持两种模式：
// 1. 简单模式：直接传递证书内容（适用于小证书）
// 2. Callback 模式：通过 stream 动态获取（推荐，类似 auth_stream）
message TLSConfig {
    // 简单配置
    bool skip_verify = 1;
    
    // 方式 1: 直接传递证书内容（简单场景）
    bytes ca_cert = 2;      // CA 证书内容 (PEM 格式)
    bytes client_cert = 3;  // 客户端证书内容 (PEM 格式)
    bytes client_key = 4;   // 客户端密钥内容 (PEM 格式)
    
    // 方式 2: Callback stream（高级场景，推荐）
    string tls_stream = 5;  // TLS callback stream ID
}

// TLS 请求 (daemon -> client)
message TLSRequest {
    string host = 1;           // registry 主机名
    TLSRequestType type = 2;   // 请求的证书类型
}

enum TLSRequestType {
    CA_CERT = 0;       // 请求 CA 证书
    CLIENT_CERT = 1;   // 请求客户端证书
    CLIENT_KEY = 2;    // 请求客户端密钥
}

// TLS 响应 (client -> daemon)
message TLSResponse {
    bytes data = 1;  // PEM 格式的证书/密钥内容
    string error = 2; // 错误信息（如果有）
}
```

#### 3.4 实现 TLS Callback Stream (推荐方案)

类似 `auth_stream` 的实现，使用 callback 机制动态获取 TLS 证书：

##### 客户端侧 (MarshalAny)

```go
// core/transfer/registry/registry.go

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
        
        go func() {
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
                    }
                case transfertypes.TLSRequestType_CLIENT_CERT:
                    if r.tlsConfig.ClientCert != "" {
                        data, readErr = os.ReadFile(r.tlsConfig.ClientCert)
                    }
                case transfertypes.TLSRequestType_CLIENT_KEY:
                    if r.tlsConfig.ClientKey != "" {
                        data, readErr = os.ReadFile(r.tlsConfig.ClientKey)
                    }
                }
                
                // 构造响应
                resp := &transfertypes.TLSResponse{}
                if readErr != nil {
                    resp.Error = readErr.Error()
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
        }()
        
        // 设置 TLS 配置
        res.TlsConfig = &transfertypes.TLSConfig{
            SkipVerify: r.tlsConfig.SkipVerify,
            TlsStream:  sid,
        }
    } else if r.tlsConfig != nil && r.tlsConfig.SkipVerify {
        // 仅 skip verify，不需要 stream
        res.TlsConfig = &transfertypes.TLSConfig{
            SkipVerify: true,
        }
    }
    
    // ... 现有代码 ...
}
```

##### Daemon 侧 (UnmarshalAny)

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
        
        hostOptions.UpdateClient = func(client *http.Client) error {
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
            
            // TLS callback stream
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
                        return fmt.Errorf("failed to parse CA certificate")
                    }
                    transport.TLSClientConfig.RootCAs = caCertPool
                }
                
                // 获取客户端证书
                clientCertData, err := tlsCallback.GetCert(ctx, s.Reference, transfertypes.TLSRequestType_CLIENT_CERT)
                if err == nil && len(clientCertData) > 0 {
                    clientKeyData, err := tlsCallback.GetCert(ctx, s.Reference, transfertypes.TLSRequestType_CLIENT_KEY)
                    if err == nil && len(clientKeyData) > 0 {
                        cert, err := tls.X509KeyPair(clientCertData, clientKeyData)
                        if err != nil {
                            return fmt.Errorf("failed to load client certificate: %w", err)
                        }
                        transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
                    }
                }
            }
            
            // HTTP 调试 (现有代码)
            // ...
            
            return nil
        }
    }
    
    // ... 现有代码 ...
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
        return nil, err
    }
    
    if err := tc.stream.Send(anyType); err != nil {
        return nil, err
    }
    
    resp, err := tc.stream.Recv()
    if err != nil {
        return nil, err
    }
    
    var tlsResp transfertypes.TLSResponse
    if err := typeurl.UnmarshalTo(resp, &tlsResp); err != nil {
        return nil, err
    }
    
    if tlsResp.Error != "" {
        return nil, fmt.Errorf("TLS callback error: %s", tlsResp.Error)
    }
    
    return tlsResp.Data, nil
}
```

```go
// core/transfer/registry/registry.go

func (r *OCIRegistry) MarshalAny(ctx context.Context, sm streaming.StreamCreator) (typeurl.Any, error) {
    res := &transfertypes.RegistryResolver{
        // ... 现有字段 ...
    }
    
    // 序列化 TLS 配置
    if r.tlsConfig != nil {
        tlsConf := &transfertypes.TLSConfig{
            SkipVerify: r.tlsConfig.SkipVerify,
        }
        
        // 读取证书文件内容
        if r.tlsConfig.CACert != "" {
            data, err := os.ReadFile(r.tlsConfig.CACert)
            if err != nil {
                return nil, fmt.Errorf("failed to read CA cert: %w", err)
            }
            tlsConf.CaCert = string(data)
        }
        
        if r.tlsConfig.ClientCert != "" {
            data, err := os.ReadFile(r.tlsConfig.ClientCert)
            if err != nil {
                return nil, fmt.Errorf("failed to read client cert: %w", err)
            }
            tlsConf.ClientCert = string(data)
        }
        
        if r.tlsConfig.ClientKey != "" {
            data, err := os.ReadFile(r.tlsConfig.ClientKey)
            if err != nil {
                return nil, fmt.Errorf("failed to read client key: %w", err)
            }
            tlsConf.ClientKey = string(data)
        }
        
        res.TlsConfig = tlsConf
    }
    
    // ... 现有代码 ...
}

func (r *OCIRegistry) UnmarshalAny(ctx context.Context, sm streaming.StreamGetter, a typeurl.Any) error {
    // ... 现有代码 ...
    
    // 反序列化 TLS 配置
    if s.Resolver != nil && s.Resolver.TlsConfig != nil {
        r.tlsConfig = &TLSConfig{
            SkipVerify: s.Resolver.TlsConfig.SkipVerify,
        }
        
        // 将证书内容写入临时文件
        if s.Resolver.TlsConfig.CaCert != "" {
            tmpFile, err := os.CreateTemp("", "ca-cert-*.pem")
            if err != nil {
                return fmt.Errorf("failed to create temp CA cert file: %w", err)
            }
            defer tmpFile.Close()
            
            if _, err := tmpFile.Write([]byte(s.Resolver.TlsConfig.CaCert)); err != nil {
                return fmt.Errorf("failed to write CA cert: %w", err)
            }
            r.tlsConfig.CACert = tmpFile.Name()
        }
        
        // 类似处理 ClientCert 和 ClientKey
        // ...
        
        // 应用 TLS 配置到 hostOptions
        hostOptions.UpdateClient = func(client *http.Client) error {
            // ... TLS 配置逻辑 ...
        }
    }
    
    // ... 现有代码 ...
}
```

### 4. 更新 CLI 命令

```go
// cmd/ctr/commands/images/push.go

func (cliContext *cli.Context) error {
    // ... 现有代码 ...
    
    if !cliContext.Bool("local") {
        // 移除不支持的 flags 检查，因为现在都支持了
        
        ch, err := commands.NewStaticCredentials(ctx, cliContext, ref)
        if err != nil {
            return err
        }
        
        if local == "" {
            local = ref
        }
        
        // 构建 registry 选项
        opts := []registry.Opt{
            registry.WithCredentials(ch),
            registry.WithHostDir(cliContext.String("hosts-dir")),
        }
        
        if cliContext.Bool("plain-http") {
            opts = append(opts, registry.WithDefaultScheme("http"))
        }
        
        // TLS 配置
        if cliContext.Bool("skip-verify") {
            opts = append(opts, registry.WithSkipVerify(true))
        }
        if cliContext.IsSet("tlscacert") {
            opts = append(opts, registry.WithTLSCACert(cliContext.String("tlscacert")))
        }
        if cliContext.IsSet("tlscert") && cliContext.IsSet("tlskey") {
            opts = append(opts, registry.WithTLSClientConfig(
                cliContext.String("tlscert"),
                cliContext.String("tlskey"),
            ))
        }
        
        // HTTP 调试
        if cliContext.Bool("http-dump") {
            opts = append(opts, registry.WithHTTPDebug())
        }
        if cliContext.Bool("http-trace") {
            opts = append(opts, registry.WithHTTPTrace())
        }
        
        reg, err := registry.NewOCIRegistry(ctx, ref, opts...)
        if err != nil {
            return err
        }
        
        // 构建 image store 选项
        var p []ocispec.Platform
        if pss := cliContext.StringSlice("platform"); len(pss) > 0 {
            p, err = platforms.ParseAll(pss)
            if err != nil {
                return fmt.Errorf("invalid platform %v: %w", pss, err)
            }
        }
        
        imageOpts := []image.StoreOpt{image.WithPlatforms(p...)}
        
        // manifest 配置
        if manifest := cliContext.String("manifest"); manifest != "" {
            desc := ocispec.Descriptor{}
            desc.Digest, err = digest.Parse(manifest)
            if err != nil {
                return fmt.Errorf("invalid manifest digest: %w", err)
            }
            desc.MediaType = cliContext.String("manifest-type")
            imageOpts = append(imageOpts, image.WithManifest(desc))
        }
        
        is := image.NewStore(local, imageOpts...)
        
        // 构建 transfer 选项
        transferOpts := []transfer.Opt{transfer.WithProgress(pf)}
        
        if cliContext.Bool("allow-non-distributable-blobs") {
            transferOpts = append(transferOpts, transfer.WithAllowNonDistributableBlobs(true))
        }
        
        pf, done := ProgressHandler(ctx, os.Stdout)
        defer done()
        
        return client.Transfer(ctx, is, reg, transferOpts...)
    }
    
    // ... local 模式的现有代码 ...
}
```

## 实现步骤

### Phase 1: 基础功能 (已完成)
- [x] HTTP 调试功能 (`http-dump`, `http-trace`)
- [x] 并发上传限制 (`max-concurrent-uploaded-layers`)

### Phase 2: Manifest 支持
1. 扩展 `image.Store` 添加 `WithManifest()` 选项
2. 更新 `imagestore.proto` 添加 manifest 字段
3. 修改 `ImageStore.Get()` 方法支持指定 manifest
4. 重新生成 protobuf 代码
5. 更新 CLI 命令移除 manifest 相关的限制

### Phase 3: Non-distributable Blobs 支持
1. 扩展 `transfer.Config` 添加 `AllowNonDistributableBlobs` 字段
2. 添加 `WithAllowNonDistributableBlobs()` 选项函数
3. 修改 `push()` 方法添加过滤器逻辑
4. 创建 `transfer.proto` 定义传输选项
5. 更新 CLI 命令移除相关限制

### Phase 4: TLS 配置支持
1. 定义 `TLSConfig` 结构体
2. 添加 TLS 相关的 `Opt` 函数
3. 修改 `NewOCIRegistry()` 应用 TLS 配置
4. 更新 `registry.proto` 添加 TLS 配置
5. 实现序列化/反序列化逻辑
6. 更新 CLI 命令移除 TLS 相关限制

### Phase 5: 测试与文档
1. 添加单元测试
2. 添加集成测试
3. 更新文档说明新功能
4. 更新 CLI help 信息

## 注意事项

### 设计理念
1. **遵循原作者意图**: proto 文件中的注释 `// Force skip verify // CA callback? Client TLS callback?` 表明原作者 Derek McGowan 在设计时就考虑过使用 callback 机制
2. **与 auth_stream 保持一致**: TLS callback 应该采用与 auth_stream 相同的设计模式，通过双向流动态获取证书
3. **支持两种模式**: 
   - 简单模式：直接传递证书内容（适用于小证书、测试场景）
   - Callback 模式：通过 stream 动态获取（生产环境推荐）

### 安全性
1. **TLS 证书传输**: 通过 gRPC 传输证书内容时需要确保连接安全
2. **临时文件清理**: 反序列化时创建的临时证书文件需要及时清理
3. **权限控制**: 确保只有授权用户可以跳过 TLS 验证

### 兼容性
1. **向后兼容**: 新增字段使用 `optional` 确保旧版本兼容
2. **Protobuf 版本**: 确保 protobuf 定义与现有版本兼容
3. **API 稳定性**: 新增的 API 应该标记为实验性功能

### 性能
1. **证书缓存**: 考虑缓存已加载的证书避免重复读取
2. **并发控制**: 确保 semaphore 正确限制并发数
3. **内存使用**: 大证书文件可能占用较多内存

## 测试计划

### 单元测试
- [ ] `image.WithManifest()` 选项测试
- [ ] `transfer.WithAllowNonDistributableBlobs()` 选项测试
- [ ] TLS 配置选项测试
- [ ] Protobuf 序列化/反序列化测试

### 集成测试
- [ ] 使用指定 manifest 推送镜像
- [ ] 推送包含非分发 blobs 的镜像
- [ ] 使用自签名证书的 registry
- [ ] 跳过 TLS 验证的场景
- [ ] HTTP 调试输出验证

### 性能测试
- [ ] 并发上传限制效果测试
- [ ] 大镜像推送性能测试
- [ ] 证书加载性能测试

## 参考资料

- [containerd transfer service 设计文档](https://github.com/containerd/containerd/blob/main/docs/transfer.md)
- [Docker Registry HTTP API V2](https://docs.docker.com/registry/spec/api/)
- [OCI Distribution Spec](https://github.com/opencontainers/distribution-spec)
- [Go TLS 配置文档](https://pkg.go.dev/crypto/tls)
