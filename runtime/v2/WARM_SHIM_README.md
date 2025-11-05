# Warm Shim Prototype

## 概述

Warm Shim 是一个容器运行时优化特性，通过预先启动 shim 进程并维护一个进程池，来减少容器创建的延迟。

## 设计方案

- 设计文档：`WARM_SHIM_DESIGN.md`

## 架构设计

### 核心组件

1. **WarmPool** (`warm_pool.go`)
   - 维护预启动的 shim 进程池
   - 按 namespace + runtime 维度管理池
   - 支持动态补充和容错

2. **WarmShim** (`warm.go`)
   - 定义 warm shim 的接口和状态
   - 三种状态：Warming → Bound → Active

3. **Bind RPC** (`warm_service.go`, `warm_client.go`)
   - 将 warm shim 绑定到具体容器
   - 重定位 socket、日志、bundle 路径

4. **ShimManager 集成** (`manager.go`)
   - 在容器创建时优先使用 warm shim
   - 失败时自动降级到冷启动

## 使用方法

### 1. 配置启用 Warm Pool

在 containerd 配置文件中添加：

```toml
[plugins."io.containerd.runtime.v2.task"]
  [plugins."io.containerd.runtime.v2.task".warm_pool]
    enabled = true
    size = 2
    take_timeout = "100ms"
```

配置说明：
- `enabled`: 是否启用 warm shim 功能
- `size`: 每个 namespace+runtime 维护的 warm shim 数量
- `take_timeout`: 从池中获取 shim 的超时时间

### 2. 创建容器

使用标准 containerd API 创建容器，系统会自动使用 warm shim（如果可用）：

```go
container, err := client.NewContainer(ctx, "my-container",
    containerd.WithNewSnapshot("my-snapshot", image),
    containerd.WithNewSpec(oci.WithImageConfig(image)),
)

task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
```

### 3. 监控和日志

Warm shim 相关的日志会包含以下字段：
- `warm_id`: warm shim 的临时 ID
- `bound_id`: 绑定后的容器 ID
- `runtime`: 使用的运行时名称

示例日志：
```
INFO[...] created warm pool namespace=default runtime=io.containerd.runc.v2
INFO[...] using warm shim from pool id=my-container
INFO[...] warm shim successfully bound to container bound_id=my-container warm_id=warm-default-1234567890
```

## 生命周期

### Warm Shim 生命周期

```
┌─────────────┐  warmstart   ┌──────────┐  Bind RPC  ┌────────┐  Create  ┌────────┐
│  Pool Init  │ ─────────────>│ Warming  │ ─────────>│ Bound  │ ───────>│ Active │
└─────────────┘               └──────────┘            └────────┘          └────────┘
                                    │                                          │
                                    │ timeout/error                            │
                                    ↓                                          ↓
                              ┌──────────┐                              ┌──────────┐
                              │ Cleanup  │<─────────────────────────────│   Exit   │
                              └──────────┘     container finished        └──────────┘
```

### 详细流程

1. **Pool 初始化**
   - ShimManager 启动时，如果配置启用，创建 warm pool
   - 预先启动配置数量的 warm shim

2. **Warm 启动**
   - 调用 `containerd-shim-runc-v2 warmstart`
   - 创建临时 bundle 目录：`/run/containerd/warm/<namespace>/<warm-id>/`
   - 初始化 socket 和日志管道
   - 进入等待状态

3. **容器创建请求**
   - containerd 接收创建容器请求
   - 尝试从 pool 获取 warm shim（带超时）
   - 如果获取成功，调用 Bind RPC

4. **Bind 操作**
   - 将 warm shim 绑定到真实容器 ID
   - 移动 socket 和相关文件到正式 bundle 目录
   - 更新 shim 内部状态

5. **Create 阶段**
   - 调用 TaskService.Create
   - 执行 `runc create`
   - 容器进入运行状态

6. **容器退出**
   - 按标准流程处理退出
   - shim 进程退出，不回池

## 容错机制

### Fallback 到冷启动

以下情况会自动降级到传统冷启动：

1. Pool 配置未启用
2. Pool 中无可用 shim（超时）
3. Bind 操作失败
4. 任何 warm 相关错误

示例日志：
```
WARN[...] failed to bind warm shim, falling back to cold start error="..."
INFO[...] startShim: beginning shim startup process task_id=my-container
```

### 自动补充

- 从 pool 取出 shim 后，后台异步创建新的 warm shim 补充
- 补充失败会记录警告，不影响当前操作

## 性能考虑

### 预期优化

- **启动延迟**: 减少 shim 进程启动时间（通常 10-50ms）
- **资源预分配**: 避免冷启动时的资源分配开销
- **并发能力**: 多个容器同时创建时更平滑

### 资源开销

- **内存**: 每个 warm shim 约 5-10MB
- **CPU**: 空闲时几乎无开销
- **存储**: 每个 warm shim 约 100KB（socket + 日志文件）

### 推荐配置

| 场景 | Pool Size | Take Timeout |
|------|-----------|--------------|
| 低频创建 | 1-2 | 100ms |
| 中频创建 | 2-5 | 200ms |
| 高频创建 | 5-10 | 500ms |

## 实现状态

### 已完成

- ✅ WarmPool 基础实现
- ✅ warmstart 子命令支持
- ✅ Bind RPC 框架（原型模式）
- ✅ ShimManager 集成
- ✅ 自动 fallback 机制
- ✅ 异步补充机制

### 待完善（生产环境）

- ⏳ 完整的 ttrpc proto 定义
- ⏳ Bind RPC 的完整实现
- ⏳ 空闲 shim 回收机制
- ⏳ 更细粒度的指标监控
- ⏳ 压力测试和性能基准
- ⏳ 多运行时支持测试

## 故障排查

### 问题：Warm shim 绑定失败

```
ERROR failed to bind warm shim error="..."
```

**可能原因**：
- Bundle 目录权限问题
- Socket 文件移动失败
- 内存不足

**解决方法**：
- 检查文件系统权限
- 查看 shim 日志详细信息
- 系统会自动回退到冷启动

### 问题：Pool 补充失败

```
WARN failed to refill warm pool error="..."
```

**影响**：
- 不影响当前操作
- 可能导致后续请求无法使用 warm shim

**解决方法**：
- 检查系统资源（内存、进程数限制）
- 检查 runtime 二进制是否可执行
- 考虑降低 pool size

### 问题：Timeout 获取 warm shim

```
DEBUG timeout waiting for warm shim
```

**原因**：
- Pool 为空且补充未完成
- Take timeout 设置过小

**解决方法**：
- 增大 `take_timeout`
- 增大 `size`
- 正常情况，会自动回退到冷启动

## 开发指南

### 添加新的 Runtime 支持

Warm shim 理论上支持所有 v2 runtime，但需要确保：

1. Runtime 支持 `warmstart` 子命令
2. Runtime 实现 Bind RPC 服务
3. 测试 warmstart 模式下的行为

### 扩展 Bind 协议

如需在 Bind RPC 中传递更多信息：

1. 修改 `WarmBindRequest` 结构体（`warm.go`）
2. 更新 shim 端的 `Bind` 实现（`warm_service.go`）
3. 更新 containerd 端的调用（`warm_pool.go`）

## 参考资料

- [Containerd Shim v2 Protocol](../README.md)
- [Runtime v2 Architecture](../../docs/PLUGINS.md)
- [Task Service API](../../api/runtime/task/v2/)

## 许可证

Apache License 2.0

