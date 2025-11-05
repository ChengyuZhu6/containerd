# Warm Shim Pool 设计方案（原型）

> 基于 commit `e3e7ec6f22f67a7ab7e77811105eba39fd23b8b6` 的实现与现有原型文档整理。
> 
> 注意：当前实现为**原型**，`Bind` RPC 仍为模拟调用（未完成真实 ttrpc proto/注册/调用链）。本文将同时说明“已实现行为”和“面向生产的目标设计”。

## 1. 背景与动机

在 containerd Runtime v2 模式下，容器创建需要启动 shim 进程（例如 `containerd-shim-runc-v2`），并建立与 shim 的连接、日志管道与 socket 文件等。

在“短生命周期 / 高频创建”的工作负载（CI/CD、Serverless、批处理、弹性扩缩等）中，shim 冷启动的固定开销会显著影响容器创建延迟。

**Warm Shim Pool** 的核心思路：
- 预先启动一定数量的 shim 进程（warmstart），形成池；
- 当容器创建请求到来时，优先从池中取出一个 warm shim，并将其绑定到真实容器（Bind）；
- 任何 warm 相关失败都自动降级为冷启动，保证可用性优先。

## 2. 目标 / 非目标

### 2.1 目标

- **降低容器创建延迟**：减少 shim 冷启动的固定开销（通常 10–50ms 量级，视环境而定）。
- **对上层透明**：不改变 containerd 对外 API，默认关闭。
- **可用性优先**：warm 失败必须可靠回退到冷启动路径。
- **隔离与可控**：按 `namespace + runtime` 维度维护池，避免跨租户/跨 runtime 影响。

### 2.2 非目标（当前原型阶段）

- 完整的 proto 定义与真实 ttrpc 服务注册/调用链。
- 空闲 warm shim 的回收策略（TTL/健康检查/动态伸缩）。
- 跨平台/多 runtime 的充分验证与兼容性覆盖。
- 完整的 Prometheus 指标与 tracing。

## 3. 术语

- **Cold Start**：按现有逻辑启动 shim（`start` action）。
- **Warm Start**：以 `warmstart` action 预启动 shim，尚未绑定到真实容器 ID。
- **Bind**：将 warm shim 从临时 ID 绑定到真实容器 ID，并完成 bundle/socket/log 等资源重定位。
- **Bundle**：shim 工作目录与相关状态文件目录（runtime v2 state 路径下）。

## 4. 总体架构

### 4.1 组件

- `runtime/v2/manager.go`
  - 在 `ShimManager.Start()` 中优先尝试 warm pool。
  - 以 `namespace/runtime` 为 key 懒加载 warm pool。

- `runtime/v2/warm_pool.go`
  - `WarmPoolConfig`：`enabled/size/take_timeout`。
  - `warmPool`：维护 `chan *warmShimInstance` 作为池。
  - `warmShimInstance`：shim 包装，具备状态机与 `Bind()`。

- `runtime/v2/binary.go`
  - `binary.StartWarm()`：以 `warmstart` action 拉起 shim。

- `runtime/v2/shim/shim.go`
  - 新增 `warmstart` action：启动 shim 但不绑定特定容器。

- `runtime/v2/shim/warm_service.go` + `runtime/v2/shim/warm_plugin.go`
  - shim 侧 WarmService（原型）：负责 Bind 后的资源重定位。

- `runtime/v2/warm_client.go`
  - containerd 侧 WarmClient（原型）：当前仅模拟 Bind RPC 成功返回。

### 4.2 数据与控制流（高层）

1. **池初始化（懒加载）**：第一次在某个 `namespace+runtime` 下创建容器时，创建并启动 pool（预热 N 个 warm shim）。
2. **容器创建**：从 pool `Take()` 一个 warm shim（带超时）；成功则调用 `Bind()`；失败则回退 cold start。
3. **异步补充**：每次 `Take()` 成功后，后台异步 `warmOne()` 补充回池。

## 5. 关键设计决策

### 5.1 池粒度：`namespace + runtime`

理由：
- namespace 是天然的多租户隔离边界；
- runtime 维度可能带来不同的启动参数与行为差异；
- 便于未来按 runtime 做不同的 pool 策略（大小、TTL、健康检查等）。

### 5.2 Bind 时机：请求到来时绑定

理由：
- 避免预分配容器 ID；
- 降低状态管理复杂度；
- 遵循“按需使用”的资源策略。

### 5.3 失败策略：任何 warm 失败都回退 cold start

理由：
- 确保功能可用性；
- 不把 warm 的不确定性暴露给用户；
- 降低引入新 feature 的风险。

## 6. 详细设计

### 6.1 配置模型

`WarmPoolConfig`（见 `runtime/v2/warm_pool.go`）：
- `enabled`：是否启用 warm pool（默认 false）。
- `size`：每个 `namespace+runtime` 的池大小（默认 2）。
- `take_timeout`：从池中等待 warm shim 的超时时间（默认 100ms）。

示例：见 `runtime/v2/warm_shim_example.toml`。

### 6.2 Warm shim 状态机

- `Warming`：warmstart 已启动，尚未绑定。
- `Bound`：完成 Bind，已切换到真实容器 ID/Bundle。
- `Active`：容器运行中（原型中未显式推进到 Active；为后续扩展预留）。

约束：
- 仅允许在 `Warming` 状态执行 `Bind()`；否则返回错误。

### 6.3 Pool 初始化与预热

触发点：`ShimManager.Start()` 在非 sandbox task 分支中调用 `getWarmPool()`。

`getWarmPool()`：
- 若 `WarmPoolConfig.Enabled` 为 false，直接返回 nil。
- 读取 namespace，构造 key：`<namespace>/<runtime>`。
- 若不存在则创建 `newWarmPool()` 并 `pool.Start()`。

`pool.Start()`：
- 循环 `size` 次调用 `warmOne()` 启动 warm shim 并放入 channel。
- 单个 warm 失败仅记录 warning，不阻塞整体。

### 6.4 warmOne：创建临时 bundle 并 warmstart

`warmOne()`：
- 生成临时 ID：`warm-<namespace>-<unixNano>`。
- 在 state 目录下创建临时 bundle：`<state>/warm/<namespace>/<warm-id>/`（权限 0700）。
- 通过 `binary.StartWarm()` 以 `warmstart` action 启动 shim。
- 成功后将 `warmShimInstance` 放入 `pool.shims`（非阻塞）；如池满则关闭 shim 并清理 bundle。

### 6.5 Take：获取 warm shim + 异步补充

`Take(ctx)`：
- 为当前 ctx 增加 `take_timeout`。
- `select` 从 channel 取 shim；若超时则返回 nil 并记录 debug log。
- 取到后立即启动后台 goroutine 异步 `warmOne()` 补充。

并发考虑：
- `pool.shims` 使用带缓冲 channel 控制最大容量。
- `pool.closed` 通过互斥锁保护。
- 取出后补充与前台绑定/创建解耦，避免影响当前请求延迟。

### 6.6 Bind：从 warm shim 绑定到真实容器

在 `ShimManager.Start()` 中，获取到 `warmShim` 后：
- 调用 `warmShim.Bind(ctx, id, opts)`；失败则关闭并回退到 cold start。
- Bind 成功则将 shim 加入 `m.shims`（NSMap）并返回。

Bind 的关键动作：
- 计算真实 bundle 目标路径：从当前 warm bundle 向上回溯到 stateDir，再拼接 `/<namespace>/<container-id>`。
- 调用 warm shim 的 Bind RPC（面向生产应完成 socket/address/log/bundle 的迁移）。
- 更新 `shim.bundle.ID` 和 `shim.bundle.Path` 为真实容器信息。

现状说明（原型）：
- containerd 侧 `WarmClient.Bind()` 为模拟实现，直接返回 `Ready=true`。
- shim 侧 `WarmService` 具备 move `address` 文件、`chdir` 等逻辑，但当前 `RegisterWarmService()` 为 no-op，尚未形成真实 RPC 可调用链。

### 6.7 Fallback（降级）

降级触发条件（任一）：
- `WarmPoolConfig.Enabled=false`。
- pool 取不到 shim（超时/关闭）。
- `Bind()` 或 warm 相关步骤出错。
- pool 启动失败。

降级行为：
- 走原有 `startShim()` 冷启动路径（`binary.Start()`）。

### 6.8 资源清理

- warm bundle 创建失败/启动失败：`os.RemoveAll(warmBundlePath)`。
- pool 满：关闭 shim 并清理 bundle。
- pool 关闭：关闭 channel，遍历剩余 warm shim，逐个 `Close()` 并清理 bundle。

## 7. 可观测性（日志）

关键日志点：
- 创建 pool：`created warm pool`（带 `namespace`, `runtime`）。
- 使用 warm shim：`using warm shim from pool`（带容器 `id`）。
- 取出 warm shim：`took warm shim from pool`（带 `warm_id`）。
- Bind 成功：`warm shim successfully bound`。
- Bind 失败回退：`failed to bind warm shim, falling back to cold start`。
- Take 超时：`timeout waiting for warm shim`。

后续建议：
- 增加 metrics：pool size、available、take latency、fallback count、warmstart failure count 等。
- 在日志中明确 `namespace/runtime` 与 warmID 关联。

## 8. 兼容性与风险评估

- 默认关闭，不影响现有行为。
- 未引入对外 API 变更，仅新增配置项。
- 风险主要集中在：
  - 真实 Bind 资源重定位实现（socket/log/bundle）正确性；
  - 并发/异常场景下资源泄漏（残留 bundle/socket/shim 进程）；
  - 多 runtime / sandbox task / grouping 机制兼容性。

## 9. 测试计划（建议）

- 单元测试：
  - `Take()` 超时、Take+Refill 并发、Close 行为、pool 满处理。
  - `Bind()` 状态机校验、Bind 失败回退。

- 集成测试：
  - 启用 warm pool 后连续创建容器，观察日志是否命中 warm 路径。
  - 模拟 Bind 失败与 warmstart 失败，验证 cold start 回退可用。

- 性能测试：
  - A/B：关闭 vs 开启（不同 size/take_timeout）下的容器创建 p50/p95。

## 10. 面向生产的下一步工作（TODO）

1. **完成真实 Bind RPC**
   - 定义 ttrpc proto（请求包含 bundle/socket/log 迁移所需信息）。
   - shim 侧注册 WarmService（`RegisterWarmService` 改为真实注册）。
   - containerd 侧 WarmClient 改为真实 ttrpc client 调用。

2. **完善资源重定位**
   - address/socket、日志 FIFO、bundle 目录与相关文件的原子迁移与失败回滚。

3. **池维护能力**
   - 空闲回收（TTL）、健康检查、启动失败退避、动态调整 size。

4. **可观测性**
   - Prometheus metrics + tracing；关键路径统计。

5. **兼容性验证**
   - 多 runtime 支持与行为差异评估。
   - 与 sandbox task / grouping 机制的兼容性测试。

