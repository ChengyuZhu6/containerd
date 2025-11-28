# Kata-Direct Runtime

## 概述

Kata-Direct 是一个实验性的 containerd 运行时实现，它将 Kata Containers 直接集成到 containerd 进程中，**消除了 shim 进程**，通过直接函数调用管理虚拟机。

## 架构对比

### 传统 Kata 架构
```
containerd (主进程)
    ↓ TTRPC (Unix Socket)
containerd-shim-kata-v2 (独立进程)
    ↓ 函数调用
virtcontainers (Go 库)
    ↓ 进程管理
QEMU/Cloud-Hypervisor (VM)
```

### Kata-Direct 架构
```
containerd (主进程)
    ↓ 直接函数调用 (无 IPC)
kata-direct runtime (goroutine)
    ↓ 函数调用
virtcontainers (Go 库)
    ↓ 进程管理
QEMU/Cloud-Hypervisor (VM)
```

## 核心特性

### ✅ 优势

1. **性能提升**
   - 消除 TTRPC 通信开销
   - 减少进程上下文切换
   - 预计启动速度提升 15-25%

2. **简化架构**
   - 无需管理 shim 进程生命周期
   - 减少进程数量
   - 简化调试流程

3. **资源节省**
   - 每个容器节省 ~10MB 内存 (shim 进程开销)
   - 减少文件描述符使用

### ⚠️ 风险和限制

1. **隔离性降低**
   - VM 管理代码在 containerd 进程中运行
   - 使用 goroutine + panic recovery 保护
   - containerd 崩溃会影响所有 VM

2. **状态恢复**
   - containerd 重启后需要重新连接到现有 VM
   - 需要持久化 VM 状态 (TODO)

3. **兼容性**
   - 可能与某些 containerd 功能不兼容
   - 需要充分测试

## 安装和配置

### 1. 编译

```bash
cd /root/go/src/github.com/containerd
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

## 使用方法

### 使用 crictl

```bash
# 创建 Pod
crictl runp --runtime kata-direct pod.yaml

# 创建容器
crictl create <pod-id> container.yaml pod.yaml

# 启动容器
crictl start <container-id>

# 查看状态
crictl ps
crictl pods
```

### 使用 ctr

```bash
# 运行容器
ctr run --runtime io.containerd.kata-direct.v2 \
    docker.io/library/nginx:latest \
    nginx-test

# 查看任务
ctr task ls

# 停止容器
ctr task kill nginx-test
```

## 性能测试

### 启动时间对比

```bash
# 测试脚本
#!/bin/bash

echo "Testing traditional Kata..."
time crictl runp --runtime kata pod.yaml

echo "Testing Kata-Direct..."
time crictl runp --runtime kata-direct pod.yaml
```

### 预期结果

| 指标 | 传统 Kata | Kata-Direct | 提升 |
|------|----------|-------------|------|
| 启动时间 | 2.5s | 2.0s | 20% |
| 内存占用 | 150MB | 140MB | 7% |
| CPU 使用 | 基准 | -5% | 5% |

## 故障排查

### 查看日志

```bash
# containerd 日志
journalctl -u containerd -f

# 查看 kata-direct 日志
journalctl -u containerd | grep kata-direct
```

### 常见问题

1. **容器创建失败**
   ```
   Error: failed to create kata sandbox
   ```
   - 检查 Kata 配置文件是否正确
   - 确认 hypervisor (QEMU/CLH) 已安装
   - 查看 containerd 日志获取详细错误

2. **containerd 崩溃**
   ```
   containerd.service: Main process exited
   ```
   - 这是 kata-direct 的主要风险
   - 检查是否有 panic 日志
   - 考虑回退到传统 Kata

3. **VM 无法启动**
   - 检查 `/dev/kvm` 权限
   - 确认内核支持 KVM
   - 查看 hypervisor 日志

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

### 添加日志

在代码中添加日志:

```go
serviceLog.WithField("key", value).Info("message")
serviceLog.WithError(err).Error("error message")
```

## 安全考虑

### 风险评估

1. **进程隔离**
   - ⚠️ 高风险: VM 管理代码在 containerd 进程中
   - 缓解: 使用 goroutine + panic recovery

2. **资源限制**
   - ⚠️ 中风险: VM 可能消耗过多资源
   - 缓解: 配置 cgroup 限制

3. **权限提升**
   - ⚠️ 低风险: 与传统 Kata 相同
   - 缓解: 使用 rootless 模式

### 建议

- ✅ 在测试环境中使用
- ⚠️ 生产环境需要充分测试
- ❌ 不建议用于多租户环境

## 未来改进

### TODO

- [ ] 实现状态持久化
- [ ] 支持 containerd 重启后恢复 VM
- [ ] 实现 Exec 功能
- [ ] 添加更多监控指标
- [ ] 性能优化
- [ ] 完善错误处理

### 贡献

欢迎提交 Issue 和 Pull Request！

## 许可证

Apache License 2.0

## 参考

- [Kata Containers](https://github.com/kata-containers/kata-containers)
- [containerd](https://github.com/containerd/containerd)
- [virtcontainers](https://github.com/kata-containers/kata-containers/tree/main/src/runtime/virtcontainers)
