# Kata-Direct Runtime

## 概述

Kata-Direct 是一个实验性的 containerd 运行时实现，它将 Kata Containers 直接集成到 containerd 进程中，**消除了 shim 进程**，通过直接函数调用管理虚拟机。

## 架构设计

### 传统 Kata 架构 vs Kata-Direct 架构

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              传统 Kata 架构                                  │
├─────────────────────────────────────────────────────────────────────────────┤
│  containerd (主进程)                                                         │
│       │                                                                      │
│       ▼ TTRPC (Unix Socket) ← 进程间通信开销                                  │
│  containerd-shim-kata-v2 (独立进程) ← 每个 Pod 一个 shim 进程                  │
│       │                                                                      │
│       ▼ 函数调用                                                             │
│  virtcontainers (Go 库)                                                      │
│       │                                                                      │
│       ▼ 进程管理                                                             │
│  QEMU/Cloud-Hypervisor (VM)                                                  │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│                            Kata-Direct 架构                                  │
├─────────────────────────────────────────────────────────────────────────────┤
│  containerd (主进程)                                                         │
│       │                                                                      │
│       ▼ 直接函数调用 (无 IPC) ← 消除通信开销                                   │
│  kata-direct runtime (goroutine) ← 无额外进程                                │
│       │                                                                      │
│       ▼ 函数调用                                                             │
│  virtcontainers (Go 库)                                                      │
│       │                                                                      │
│       ▼ 进程管理                                                             │
│  QEMU/Cloud-Hypervisor (VM)                                                  │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 代码模块结构

```
runtime/v2/kata-direct/
├── plugin.go     # 插件注册、工厂模式、SIGHUP 信号处理
├── service.go    # 核心服务实现，实现 TaskService 接口
├── create.go     # 容器/沙箱创建逻辑
├── start.go      # 容器启动、IO 流处理
├── delete.go     # 容器删除、资源清理
├── exec.go       # 容器内进程执行 (exec)
└── stats.go      # 容器统计信息 (CPU/Memory/IO)
```

### 核心组件交互

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              service 结构                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐                   │
│  │   sandbox    │◄───│   service    │───►│  containers  │                   │
│  │ (VC.Sandbox) │    │   (核心)     │    │ (map[id]*c)  │                   │
│  └──────────────┘    └──────────────┘    └──────────────┘                   │
│         │                   │                   │                            │
│         │                   │                   ▼                            │
│         │                   │            ┌──────────────┐                   │
│         │                   │            │  container   │                   │
│         │                   │            ├──────────────┤                   │
│         │                   │            │ - id         │                   │
│         │                   │            │ - cType      │                   │
│         │                   │            │ - exitCh     │                   │
│         │                   │            │ - execs[]    │                   │
│         │                   │            │ - IO streams │                   │
│         │                   │            └──────────────┘                   │
│         ▼                   ▼                                                │
│  ┌─────────────────────────────────────┐                                    │
│  │         virtcontainers 库            │                                    │
│  │  (CreateSandbox, StartVM, etc.)     │                                    │
│  └─────────────────────────────────────┘                                    │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 已实现功能

| 功能 | 状态 | 说明 |
|------|------|------|
| Sandbox 创建/删除 | ✅ | 支持创建和销毁 Kata sandbox |
| Container 创建/删除 | ✅ | 支持在 sandbox 中创建和删除容器 |
| Container 启动/停止 | ✅ | 支持启动和停止容器进程 |
| IO 流处理 | ✅ | stdin/stdout/stderr FIFO 支持 |
| Exec 进程 | ✅ | 支持在容器内执行新进程 |
| Stats 统计 | ✅ | 支持 CPU/Memory/IO 统计 (cgroupv1/v2) |
| 信号处理 | ✅ | 支持向容器发送信号 (Kill) |
| Panic 恢复 | ✅ | goroutine 级别的 panic 保护 |
| SIGHUP 处理 | ✅ | 防止 VM 关闭后 containerd 崩溃 |

---

## 核心特性

### 优势

1. **性能提升**
   - 消除 TTRPC 进程间通信开销
   - 减少进程上下文切换
   - 预计启动速度提升 15-25%

2. **简化架构**
   - 无需管理 shim 进程生命周期
   - 减少系统进程数量
   - 简化调试和问题排查

3. **资源节省**
   - 每个 Pod 节省 ~10MB 内存 (shim 进程开销)
   - 减少文件描述符使用

### 风险和限制

1. **隔离性降低**
   - VM 管理代码运行在 containerd 进程中
   - 使用 goroutine + panic recovery 保护
   - containerd 崩溃会影响所有运行中的 VM

2. **状态恢复 (TODO)**
   - containerd 重启后无法自动重连现有 VM
   - 需要实现 VM 状态持久化

3. **兼容性**
   - 可能与某些 containerd 功能不完全兼容
   - 需要在生产环境充分测试

---

## 并发控制设计

### 锁机制

```go
service struct {
    mu        sync.RWMutex    // 保护 sandbox 和 containers
    ioMu      sync.Mutex      // 每个 container 的 IO 锁
    execMu    sync.RWMutex    // 每个 container 的 exec 进程锁
    exitOnce  sync.Once       // 确保 exitCh 只关闭一次
}
```

### IO 生命周期管理

```
1. 从 sandbox.IOStream() 获取 streams
2. 打开 FIFO 文件
3. 启动 copy goroutines
4. 进程退出时，stdout/stderr 收到 EOF
5. stdout goroutine 关闭 stdin FIFO
6. 所有 goroutines 退出，exitIOch 关闭
7. 未使用的 streams 自动清理 (防止泄漏)
```

---

## 安装和配置

### 1. 编译

```bash
cd /path/to/containerd
make BUILDTAGS="kata_direct"
```

### 2. 配置 containerd

编辑 `/etc/containerd/config.toml`:

```toml
version = 2

[plugins."io.containerd.grpc.v1.cri".containerd]
  default_runtime_name = "kata-direct"

  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-direct]
    runtime_type = "io.containerd.kata-direct.v2"
    
    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-direct.options]
      # Kata 配置文件路径
      ConfigPath = "/etc/kata-containers/configuration.toml"
```

### 3. 重启 containerd

```bash
systemctl restart containerd
```

---

## 使用方法

### 使用 crictl

```bash
# 创建 Pod
crictl runp --runtime kata-direct pod.yaml

# 创建容器
crictl create <pod-id> container.yaml pod.yaml

# 启动容器
crictl start <container-id>

# 在容器内执行命令
crictl exec <container-id> /bin/sh

# 查看容器统计
crictl stats <container-id>

# 查看状态
crictl ps
crictl pods

# 停止和删除
crictl stop <container-id>
crictl rm <container-id>
crictl stopp <pod-id>
crictl rmp <pod-id>
```

### 使用 ctr

```bash
# 运行容器
ctr run --runtime io.containerd.kata-direct.v2 \
    docker.io/library/nginx:latest \
    nginx-test

# 查看任务
ctr task ls

# 执行命令
ctr task exec --exec-id exec1 nginx-test /bin/sh

# 停止容器
ctr task kill nginx-test
```

---

## 性能测试

### 启动时间对比

```bash
#!/bin/bash

echo "Testing traditional Kata..."
time crictl runp --runtime kata pod.yaml

echo "Testing Kata-Direct..."
time crictl runp --runtime kata-direct pod.yaml
```

### 预期结果

| 指标 | 传统 Kata | Kata-Direct | 提升 |
|------|----------|-------------|------|
| Pod 启动时间 | 2.5s | 2.0s | ~20% |
| 内存占用 (per Pod) | 150MB | 140MB | ~7% |
| CPU 使用 | 基准 | -5% | ~5% |

---

## 故障排查

### 查看日志

```bash
# containerd 日志
journalctl -u containerd -f

# 过滤 kata-direct 日志
journalctl -u containerd | grep kata-direct

# 查看详细调试日志
journalctl -u containerd --since "5 min ago" -o json-pretty
```

### 常见问题

#### 1. 容器创建失败

```
Error: failed to create kata sandbox
```

**排查步骤:**
- 检查 Kata 配置文件 `/etc/kata-containers/configuration.toml`
- 确认 hypervisor (QEMU/CLH) 已正确安装
- 验证 `/dev/kvm` 设备存在且有权限
- 查看 containerd 日志获取详细错误

#### 2. containerd 崩溃

```
containerd.service: Main process exited
```

**排查步骤:**
- 检查日志中的 panic 信息
- 确认是否是 kata-direct 相关的崩溃
- 考虑临时回退到传统 Kata shim
- 收集 coredump 进行分析

#### 3. VM 无法启动

**排查步骤:**
- 检查 `/dev/kvm` 权限: `ls -la /dev/kvm`
- 确认内核支持 KVM: `lsmod | grep kvm`
- 查看 hypervisor 日志
- 检查 SELinux/AppArmor 策略

#### 4. IO 问题

```
failed to open stdin fifo
```

**排查步骤:**
- 检查 FIFO 文件路径是否存在
- 确认文件权限正确
- 查看是否有文件描述符泄漏

---

## 开发和调试

### 启用调试日志

```toml
# /etc/containerd/config.toml
[debug]
  level = "debug"
```

### 使用 delve 调试

```bash
# 编译 debug 版本
go build -gcflags="all=-N -l" -o containerd ./cmd/containerd

# 启动 delve
dlv exec ./containerd -- --config /etc/containerd/config.toml
```

### 代码中添加日志

```go
s.log.WithField("container", c.id).Info("message")
s.log.WithError(err).Error("error message")
s.log.WithFields(logrus.Fields{
    "sandbox": sandboxID,
    "container": containerID,
}).Debug("detailed info")
```

---

## 安全考虑

### 风险评估

| 风险类型 | 级别 | 说明 | 缓解措施 |
|----------|------|------|----------|
| 进程隔离 | 高 | VM 管理代码在 containerd 中运行 | goroutine + panic recovery |
| 资源限制 | 中 | VM 可能消耗过多资源 | 配置 cgroup 限制 |
| 权限提升 | 低 | 与传统 Kata 相同 | 使用 rootless 模式 |
| 信号处理 | 低 | SIGHUP 被全局忽略 | 不影响 containerd 核心功能 |

### 使用建议

| 环境 | 建议 |
|------|------|
| 开发/测试环境 | ✅ 推荐使用 |
| 预生产环境 | ⚠️ 需充分测试 |
| 生产环境 | ⚠️ 谨慎评估风险 |
| 多租户环境 | ❌ 不建议使用 |

---

## 未来改进计划

### TODO

- [ ] **状态持久化** - 将 VM 状态保存到磁盘
- [ ] **重启恢复** - containerd 重启后重连现有 VM
- [ ] **健康检查** - 周期性 sandbox 健康检查机制
- [ ] **监控指标** - 添加 Prometheus metrics
- [ ] **资源限制** - 限制同时运行的 sandbox 数量
- [ ] **单元测试** - 完善测试覆盖

### 已完成

- [x] 基础容器生命周期管理
- [x] IO 流处理
- [x] Exec 进程支持
- [x] Stats 统计 (cgroupv1/v2)
- [x] IO 泄漏修复
- [x] exitCh 双重关闭保护
- [x] SIGHUP 信号处理

---

## 贡献

欢迎提交 Issue 和 Pull Request！

---

## 许可证

Apache License 2.0

---

## 参考链接

- [Kata Containers](https://github.com/kata-containers/kata-containers)
- [containerd](https://github.com/containerd/containerd)
- [virtcontainers](https://github.com/kata-containers/kata-containers/tree/main/src/runtime/virtcontainers)