<!--
Copyright The containerd Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->

# KEP-4216：按 runtime handler 管理镜像的后续实现计划

## 1. 范围与基线

- 跟踪议题：[containerd/containerd#14252](https://github.com/containerd/containerd/issues/14252)。
- KEP：[4216 — Image pull per runtime class](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4216-image-pull-per-runtime-class)。
- 原始设计评审：[kubernetes/enhancements#4217](https://github.com/kubernetes/enhancements/pull/4217)，属于设计文档 PR，不代表功能实现完成。
- 盘点日期：2026-10-29。
- 代码基线：`43aa4ea134f3e50791b8537c48a18303f0fcc775`，已包含 PR #14314。
- 本文是实现与验收的追踪计划，不代表设计已获维护者批准，也不代表测试已通过。

目标是让同一镜像引用在不同 runtime handler 下的变体能够共存，并在拉取、查询、容器创建、恢复、删除和 GC 中保持一致。

术语约定：CRI 传递的是 RuntimeClass 对应的 `handler`，不一定是 RuntimeClass 资源名称。本文的“变体”指 handler 上下文中的镜像关联，不要求复制底层内容。

范围边界：以当前 KEP 正文定义功能需求，以 #4217 理解设计背景，以实际代码判断完成状态。KEP 把 snapshotter per RuntimeClass 列为 Non-Goal；#14252 涉及的现有 snapshotter 配置、引用与恢复仍需正确处理，但本文不新增 runtime snapshotter 配置能力。

区分三个概念：镜像元数据存在、目标 snapshotter 已 unpack、镜像属于某个 handler 变体。三者不能互相替代；尤其不能把 `ImageStatus` 的存在性统一改成 `IsUnpacked`，这会改变未 unpack 导入镜像的既有语义，并影响 lazy snapshotter 和创建时补 unpack 路径。

## 2. 已实现的基线能力

- [x] CRI API：`k8s.io/cri-api v0.37.1` 已有 `ImageSpec.runtime_handler`。
- [x] 拒绝显式传入的未知 runtime handler：[PR #14255](https://github.com/containerd/containerd/pull/14255)，2026-10-07 合并。
- [x] 解析显式 handler、默认 runtime 与旧 annotation 回退：[PR #14314](https://github.com/containerd/containerd/pull/14314)，2026-10-27 合并。
- [x] 拉取时按 handler 配置选择 platform、snapshotter 和 unpack；transfer 不支持目标组合时回退 local pull。
- [x] 保存配置的 platform 标签，并在恢复和事件更新时使用对应平台。
- [x] 已有 handler 解析、拉取后端和平台恢复相关单元测试。

以上是拉取路径及平台信息的支持，不等于完整的 runtime-handler 变体生命周期支持。

## 3. 剩余缺口

| 编号 | 事项 | 当前状态与缺口 |
| --- | --- | --- |
| R1 | 按 `(reference/ID, handler)` 索引 | 内存 store 仍为 `reference → ID`、`ID → Image`，没有 handler 维度。 |
| R2 | 变体持久化与恢复 | 已有 platform 标签，但没有完整的 handler 变体关联；恢复时 snapshotter 检查仍使用 image plugin 的默认值。 |
| R3 | 变体事件同步 | 已有 create/update/delete 同步，但仍按 reference 更新，缺少多变体隔离。 |
| R4 | CRI 查询、列举、删除 | `ImageStatus` 忽略 handler；`ListImages`、`StreamImages` 未返回 `Spec.RuntimeHandler`；`RemoveImage` 未限定 handler。 |
| R5 | 多变体的 GC 引用关系 | 需要保证删除一个变体不影响其他变体仍引用的 content/snapshots；底层 GC 已存在。 |
| R6 | functional/e2e 闭环 | 缺少多变体共存、重启恢复、隔离删除及 kubelet GC 的完整验证。 |

关联消费端也必须联动：`CreateContainer` 当前按镜像名调用 `LocalResolve`，`toContainerdImage` 使用默认平台的 `GetImage`；sandbox 镜像使用链路也需要核对。

## 4. 按独立可用功能组织 PR

### 4.1 拆分原则

PR 是功能交付和追踪的单位，不是技术分层的单位。

- 每个 PR 合并后，用户可以通过现有入口使用它承诺的完整功能。
- 可以依赖已经合并的 PR，但不能依赖未来 PR 才能工作。
- 同一功能所需的存储、持久化、调用链、兼容处理、测试和文档必须一起交付。
- 仅添加 store、元数据或接口占位，不接通实际使用链路，不作为独立功能 PR。
- 不单独发布“能列举变体但不能安全删除”“能拉取但重启丢失关联”等中间状态。
- 测试和文档属于相应功能 PR，不留到最后一个 PR 才补齐。
- PR 内可以按依赖顺序组织提交以便审查，但内部提交和待办分组不等于可单独合并的 PR。

推荐两个 containerd PR：先修复已选镜像在消费时的身份/平台一致性，再交付 handler 变体完整生命周期。前者必须通过复现证明能在现有模型上独立修复；若实际依赖变体存储，就并入后者，不提交半成品。R1–R5 及对应测试保持为一个变体功能闭环。

### 4.2 PR 追踪表

以下编号为本文的计划编号，不是 GitHub PR 编号。PR 数量服从功能边界，不是预先规定的配额。

| 计划 PR | 独立可用功能 | 基础依赖 | 状态 | 负责人 | GitHub PR / 验收记录 |
| --- | --- | --- | --- | --- | --- |
| PR-1 | 已选镜像在容器/sandbox 消费中的身份与平台一致性修复 | 当前 main 已合入的 #14255、#14314 | 实现已提交；专项验证通过，全套验证有阻塞；待人工审查 | 未分配 | 本地 commits：`312058228`、`1aa5f0c9b`、`a6dcedca9`；未创建远程 PR |
| PR-2 | 按 runtime handler 管理镜像变体的完整生命周期 | 当前 main；复用已合入的 PR-1 修复，或将必要修复纳入本 PR | 待设计确认 | 未分配 | — |

PR-1 不提供独立 handler 归属、列举和删除。PR-2 不把核心恢复、消费、删除或 GC 正确性留给后续 PR。Kubernetes 侧必要的联动另行追踪，不计为固定数量的 containerd PR。

### 4.3 PR-1：已选镜像的消费一致性修复

**用户收益：**在现有单 reference 模型下，拉取并解析出镜像后，创建容器/sandbox 使用同一镜像配置、平台和 rootfs，而不是重新按默认平台选择另一个 manifest。

**性质：**独立缺陷修复，不宣称实现了 KEP 的 handler 变体生命周期，也不承诺同名多变体已经隔离。

#### 特性与实现待办

- [x] 建立失败用例，确认 `toContainerdImage` 及按需 unpack 中默认平台选择会造成身份/平台不一致。
- [x] 按已解析 config digest 固定消费 manifest，保持配置与 rootfs 一致；不修改持久化镜像 target。
- [x] `CreateContainer` 的镜像使用和补 unpack 保持同一消费对象；元数据/NRI 继续记录原 target digest。
- [x] sandbox 根据已有平台标签选择镜像；无标签保持默认行为。
- [x] 复用既有 snapshotter 选择规则，验证 native/overlayfs；未新增配置或改变优先级。
- [x] 保持 `ImageStatus` 的镜像存在语义，不以目标 snapshotter 的 `IsUnpacked` 代替 handler 归属。
- [x] 新增一个配置/rootfs 身份单测和一个真实 CRI 消费回归；复用现有默认平台、导入及镜像卷测试。
- [ ] lazy snapshotter、`DiscardUnpackedLayers` 实际环境验证；已静态确认 helper 不检查所选 layers 完整性，但不能替代实际验证。
- [x] 本文记录实现与测试；全套失败、跳过及环境限制保留为阻塞，不标记整体完成。

#### 独立验收与合入条件

1. 当前 main 上有可复现的问题，PR 修复后同一用例通过。
2. 已选镜像的 CRI ID、读取的配置和创建所用 rootfs 一致，不因默认平台重选发生偏移。
3. 必要的 unpack 与 sandbox 路径正确；既有导入镜像可见性和默认平台行为不回归。
4. 不合入 PR-2，以上修复仍然正确、可使用且可验证。

若复现不成立或修复必须先有 handler 变体存储，取消这个独立 PR，并把确实必要的消费改动纳入 PR-2。不是为了凑两个 PR 而保留它。

#### 2026-10-29 实现与验证记录

实现 commits（按顺序）：

1. `312058228`：复现配置/rootfs 被默认平台重选，修复镜像转换并添加一个必要单测。
2. `1aa5f0c9b`：按已解析 config digest 固定消费 manifest，接通补 unpack 和 sandbox 平台标签；添加一个真实 CRI 消费回归。
3. `a6dcedca9`：保持容器元数据/NRI 的原 target digest 和镜像卷原目录键；跳过未下载的不相关子 index，扩展已有单测。

测试基线：Linux amd64 当前节点，Kubernetes/kubelet `v1.29.1`，runc `v1.3.3`。最终节点与专项验证使用 `a6dcedca9` 构建；未修改 `go.mod`，使用缓存工具链及 `no_btrfs` 构建选项，不覆盖 btrfs snapshotter。

| 验证 | 实际结果 | 本地证据（未提交的 `_output/pr1/`） |
| --- | --- | --- |
| 修复前失败复现 | helper 配置/rootfs digest 不一致；真实 CRI 容器补 unpack 找不到 parent，sandbox 使用错误 rootfs | `helper-before.log`、`platform-before.log` |
| CRI 内部及插件全部包测试 | 通过 | `cri-package-final.log` |
| 修改包 race 测试 | server、podsandbox、opts 通过 | `race-tests.log` |
| `make check` | 通过，0 issues | `make-check-final.log` |
| 最终专项集成测试 | 11 个顶层测试全部通过；SELinux 子项因未启用而跳过 | `integration-final.log`、`integration-final.exit` |
| 当前 kubelet 工作负载验证 | 默认及 native RuntimeClass：init/普通容器、卷、exec/logs/copy、DNS、跨 Pod 网络、containerd/kubelet 重启后 ID/重启次数保持；通过 | `k8s-node-e2e.log`、`k8s-node-e2e.exit` |
| 最终完整 critest v1.35.0（无 focus/skip 过滤） | 97 passed、2 failed、14 skipped；未通过 | `critest-final.log`、`critest-final.exit` |
| 首轮完整仓库 CRI integration（无过滤） | 42 个顶层通过、10 个已结项失败、22 个顶层跳过；restart 用例报错后挂起，被终止，另一个 timeout 测试暂停未完成 | `cri-integration-full.log`、`cri-integration-full.exit` |
| 非镜像卷失败的精确基线对照 | 用 `43aa4ea` 及相同配置复测 6 个顶层失败场景，均重现失败 | `integration-baseline.log`、`integration-baseline.exit` |

合成多平台消费测试使用宿主可执行的 busybox/pause 二进制，但配置平台和 marker layer 不同，验证的是配置/rootfs 选择，不代表实际跨 CPU 执行或 Windows guest 验证。当前 kubelet 工作负载测试也不是 Kubernetes 官方 `e2e-node` 全套，且不能验证后续 PR-2 的 handler 变体隔离。

未解决的全套验证阻塞：

- critest host-port：节点 legacy iptables 缺少 `ip_tables`/NAT 支持；原运行时同一场景也失败。
- critest userns：最终版本在 native snapshotter 下报 `container ID 100000 cannot be mapped to a host ID`；原运行时同一 spec 更早报 unpack 错误，不能认定映射错误已经完成等价基线排除。
- 首轮 integration 的 exec-drain 缺少标准测试的 `10s` 配置；停止信号及取消测试在精确基线同样失败，存在脚本准备时序窗口，但未证明其具体因果时序。
- Docker daemon 不可连接、loopback-v2 挂载目标是目录、failpoint runtime 未配置；精确基线同样失败。
- restart 测试需要按进程名定位及自动重启 harness；首轮传绝对路径给 `pkill -x` 后失败并挂起。未为测试修改现有用例，也未执行可能误杀生产实例的全局清理。
- 首轮镜像卷失败包含本次中间实现的目录键回归及 native/overlayfs 配置不匹配；原键已在第三 commit 修复，最终以 overlayfs 配置重跑相关测试通过，不能把该回归归为既有问题。
- NRI、swap、SELinux/idmap 等存在未满足条件或跳过项；lazy snapshotter、discard 和 Windows guest 尚无实际环境覆盖。

当前节点的正式 containerd/shim/ctr 已逐字节恢复到备份版本，配置未更改，测试 namespace 已删除，节点 Ready，测试隔离实例和遗留沙箱已清理。代码已提交、专项已验证，但完整验收仍受上述失败/前置条件阻塞，不标记 PR-1 全部完成。

### 4.4 PR-2：镜像变体完整生命周期

**用户收益：**同一镜像引用可在不同 handler 下分别拉取、查询、列举和用于创建容器；重启后关联保留；删除一个 handler 的变体不影响其他 handler 的变体。

**范围：**接通 #14252 的镜像管理链路，使用现有 runtime、platform、snapshotter 配置及底层 GC。持久化、事件、容器在用信息、删除和引用保护属于本功能必要组成，不是后续增强。

#### 合入前必须确认的设计契约

这些是 PR-2 的前置设计决策，不是单独的基础设施 PR。

- [ ] 使用 runtime handler 作为身份维度，不以 snapshotter 或 RuntimeClass 资源名称代替。
- [ ] 明确空 handler 与显式默认 handler 是否归一，以及响应中的 handler 表示方式。
- [ ] 明确同一 image ID 对应多个 handler 时，reference、platform、snapshotter 和 pinned 状态的归属。
- [ ] 明确旧镜像、非 CRI 导入镜像的归属、迁移及兼容查询规则。
- [ ] 明确短 ID 查询、未知 handler 请求和配置移除后的既有变体处理方式。
- [ ] 明确内部存储键与对外 reference 的关系，避免内部名称泄漏到 `RepoTags`、`RepoDigests`，或被 remote snapshotter 当成 registry reference。
- [ ] 确认 kubelet feature gate 关闭或旧客户端不传 handler 时的查询、列举、删除兼容语义；containerd 不能仅凭空 handler 判断 feature gate 状态。
- [ ] 保持 CRI config digest ID 与实际选中 manifest 一致；恢复时不能仅凭 index digest 按默认平台重新选择。
- [ ] 分离 handler 的逻辑归属与共享 content/snapshot 的物理引用；覆盖同 snapshotter 不同 handler，以及同 ID 不同 snapshotter。
- [ ] 明确 `ImageStatus`、恢复和创建时补 unpack 的职责，兼容导入未 unpack 镜像、lazy snapshotter 和 `DiscardUnpackedLayers`。
- [ ] 核对共享 content 的 snapshot GC 标签及资源核算，不能假定删除逻辑变体一定立即释放对应磁盘空间。

#### 功能待办：身份、拉取与状态（R1/R4）

- [ ] 支持 `(reference, handler)` 和 `(ID, handler)` 索引，允许同一 ID 关联多个 handler。
- [ ] reference 合并、更新、删除、短 ID 查询及 pinned 关系遵循变体身份契约。
- [ ] `PullImage` 成功后登记对应变体，不覆盖其他 handler 的关联。
- [ ] `LocalResolve`、`GetImage` 及其调用者使用明确的 handler 上下文。
- [ ] `ImageStatus` 只返回目标变体；不存在时返回空响应，未知 handler 遵循已确认契约。

#### 功能待办：持久化与同步（R2/R3）

- [ ] 持久化同一 reference 的多个 handler 关联及必要的 platform/snapshotter 信息。
- [ ] 内存索引可以从持久化事实来源完整重建。
- [ ] `CheckImages` 按变体恢复并检查对应 platform/snapshotter 组合。
- [ ] create/update/delete 事件使用一致的变体更新逻辑，处理重复事件及外部删除。
- [ ] 正确处理并发拉取、tag 更新、部分失败及元数据/缓存一致性。
- [ ] 实现已确认的旧镜像和非 CRI 导入镜像兼容策略。

#### 功能待办：实际消费

- [ ] `CreateContainer` 根据 sandbox runtime 上下文选择正确变体。
- [ ] `toContainerdImage` 保留变体对应的平台，不无条件使用默认平台。
- [ ] 接通 sandbox 镜像确保、查询、创建及必要的按需 unpack 链路。
- [ ] `ListContainers`、`ContainerStatus` 回传正确的 `ImageID` 和 `ImageRuntimeHandler`，与镜像列举的 `(ID, handler)` 一致。
- [ ] 重启恢复后容器的实际镜像归属仍可正确回传；不能简单用当前默认 runtime 猜测旧容器的归属。
- [ ] 检查其他内部镜像查询调用者，避免遗漏无 handler 的消费路径。

#### 功能待办：列举、删除与 GC（R4/R5）

- [ ] `ListImages`、`StreamImages` 返回一致的变体条目及 `Spec.RuntimeHandler`。
- [ ] 对外 repo tags/digests 保持真实镜像引用，不泄漏内部存储名称。
- [ ] `RemoveImage` 只移除目标 handler 内该镜像的全部引用，保持删除幂等。
- [ ] 列举输出、查询和定向删除在同一 PR 内接通，不交付全局删除的中间状态。
- [ ] 正确维护共享 content、各 snapshotter 及 lease 的引用关系，复用现有 GC。
- [ ] 删除 A 后 B 所需资源仍被保留；所有引用消失后符合条件的资源可回收。
- [ ] 运行中容器、pinned 或有效 lease 仍持有的资源不得误回收。

#### 同一 PR 必须交付的测试与文档（R6）

- [ ] 单测覆盖索引、短 ID、pinned、reference 更新/删除、持久化往返和错误路径。
- [ ] functional/integration 覆盖同 ref、同 ID、不同 handler/snapshotter 的共存及隔离删除。
- [ ] 覆盖同 ref、不同 platform/ID 的选择、状态查询及容器创建。
- [ ] 覆盖重启恢复、tag 更新、外部删除、重复删除、并发拉取和失败一致性。
- [ ] 验证删除 A 后 B 仍可启动，以及最后一个变体删除后的资源保留/回收条件。
- [ ] 验证 local pull、transfer pull 及 transfer 回退路径。
- [ ] 验证空 handler、显式默认 handler、旧镜像和旧客户端兼容。
- [ ] 验证容器 `ImageID`/`ImageRuntimeHandler` 与镜像列举对应，重启后保持一致。
- [ ] 提交并在固定目标版本上运行 kubelet `RuntimeClassInImageCriApi` 开启/关闭的查询、列举及 GC 测试；上游阻塞单独记录。
- [ ] 实际验证 Windows guest-platform 场景，不能以平台 matcher 单测替代。
- [ ] 更新使用文档，说明 handler 语义、迁移、兼容边界及已知限制。
- [ ] 记录实际运行结果、跳过项及环境阻塞，不把静态审查等同于测试通过。

#### containerd 合并验收：无需后续 containerd PR 即可完成

1. 只登记 A 时，B 的 handler-scoped 查询不会错误命中 A 的变体。
2. 拉取 A/B 后，列举能区分两者，各自状态和容器/sandbox 消费正确。
3. 重启后，A/B 的身份、引用和使用行为保持一致。
4. 用显式 CRI 请求删除 A 后，B 仍可查询、列举并创建容器；重复删除 A 成功。
5. 最后一个变体删除后，按现有容器、pinned 和 lease 引用条件正确保留或回收资源。
6. 容器回传的镜像在用 tuple 与列举结果中的 `(imageID, handler)` 一致。
7. 已有默认 handler 和旧客户端行为符合确认的兼容契约。

KEP 整体验收还要求目标 kubelet 实际使用 handler 完成查询、在用判断及删除。存在上游传递问题时，不标记 Kubernetes 端到端完成；具体核对项见下一节。

### 4.5 Kubernetes 联动与验收前置条件

当前 KEP 定义的是设计目标，不足以证明目标 Kubernetes 版本已完整传递 handler。本次静态核查抓取的 Kubernetes `master` 中，`RemoveImage` 仍只构造带 `Image` 的 CRI 请求；必须对固定版本/commit 再确认，不能据此宣称所有 Kubernetes 版本均有此缺口。

- [ ] 固定目标 Kubernetes 版本/commit 和 feature gate 状态，记录 CRI API 兼容关系。
- [ ] 核对 `PullImage`、`ImageStatus` 请求的 handler，及 `ListImages` 结果的转换。
- [ ] 核对 kubelet GC 的镜像 tuple、容器在用 tuple 和 `RemoveImage` 请求是否使用一致的 handler。
- [ ] 若目标版本的删除请求丢失 handler，确认或推进 Kubernetes 侧修复，并跟踪实际 PR；不能用 containerd 猜测请求归属来掩盖问题。
- [ ] 用显式 CRI 请求验证 containerd 完整生命周期，再用已确认的 kubelet 版本完成集成验收。

源代码核查入口：[镜像 API 调用](https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/kuberuntime/kuberuntime_image.go)、[镜像 GC](https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/images/image_gc_manager.go)。执行测试时记录固定 commit，而不是仅引用可变化的 `master`。

containerd PR-2 的服务端功能应独立正确；目标 kubelet 若仍有阻塞，明确标注 Kubernetes 端到端未完成，不能把显式 CRI 测试当成 kubelet GC 已通过。

### 4.6 执行顺序与拆分约束

当前首先做的三件事：

1. 复现 PR-1 的身份/平台消费问题，验证它是否可以独立修复。
2. 并行核对目标 Kubernetes 版本的 handler 传递与 GC 行为。
3. 确认 PR-2 的默认/旧镜像兼容契约、持久化事实来源和 GC 引用模型。

这些是各功能 PR 的准备与验证任务，不作为只能被未来代码使用的基础设施 PR。

若 PR-1 边界成立，先完整合入该修复，再推进 PR-2。PR-2 内部按以下顺序推进，整个闭环完成后再合入：

**确认契约 → 索引及持久化 → 拉取/恢复/事件 → 查询/消费/容器信息 → 列举/删除/GC → 完整验收**。

如果实现或维护者审查提出更小的 PR，必须先写明用户现在可用的收益、已合入依赖、入口到生命周期的完整路径，以及不依赖任何后续 PR 的测试、文档与兼容保证。无法满足的改动保留为同一 PR 内的任务，不单独交付。

## 5. 主要代码入口

| 区域 | 文件 |
| --- | --- |
| 变体模型与索引 | `internal/cri/store/image/image.go`、`image_test.go` |
| handler 解析与内部查询 | `internal/cri/server/images/service.go`、`service_test.go` |
| 拉取、reference 登记、平台标签、事件更新 | `internal/cri/server/images/image_pull.go`、`image_pull_test.go` |
| 重启恢复 | `internal/cri/server/images/check.go` |
| CRI 查询与响应转换 | `internal/cri/server/images/image_status.go`、`image_status_test.go` |
| CRI 列举 | `internal/cri/server/images/image_list.go`、`image_stream.go` 及对应测试 |
| CRI 删除 | `internal/cri/server/images/image_remove.go` |
| 镜像事件分发 | `internal/cri/server/events.go` |
| 容器镜像消费及补 unpack | `internal/cri/server/container_create.go`、`helpers.go`、`internal/cri/opts/container.go` |
| 容器在用镜像信息 | `internal/cri/server/container_list.go`、`container_status.go` |
| sandbox 镜像消费 | `internal/cri/server/sandbox_run.go`、`internal/cri/server/podsandbox/` |
| runtime/image 配置桥接 | `plugins/cri/cri.go`、`plugins/cri/images/plugin.go` |
| 底层 GC 与引用机制 | `core/metadata/`、`client/`、`docs/garbage-collection.md` |
| 集成测试与执行说明 | `integration/`、`docs/cri/testing.md`、`BUILDING.md` |

该表是改动入口，不是必须修改的文件清单；实现时按实际依赖控制范围。

## 6. 追踪规则与完成条件

- 开始每个功能 PR 前核对 #14252 及关联 PR，避免与其他实现重复。
- 在 PR 表中补充负责人、GitHub PR 链接与实际状态：待设计确认、待开始、进行中、阻塞、完成。
- 设计决策与本文不同，以已确认的设计为准，并同步更新本文。
- 子项勾选对应实际实现或验证结果，不代表该部分可以单独合入。
- 功能 PR 完成需有合并记录及完整验收证据，不能将必要的调用链、兼容处理或测试留给后续 PR。
- 测试未运行、被跳过或受环境限制时，保留未完成状态并记录原因。
- 在独立可用、正确和可验证的前提下缩小 PR 范围；代码、文档及测试结果由人工审查后提交。

### 决策与进度记录

| 日期 | 计划 PR / 范围 | 决策 / 进展 / 阻塞 | PR / 验收证据 |
| --- | --- | --- | --- |
| 2026-10-29 | 基线 | 按 `43aa4ea` 静态核查，整理剩余 6 类缺口；未运行测试。 | PR #14255、PR #14314 |
| 2026-10-29 | PR 拆分原则（早期方案） | 取消 P0–P5 技术分层交付，改为功能闭环；最初只列一个生命周期 PR，已由下述评估修订。 | 历史决策，不作为当前 PR 编号依据 |
| 2026-10-29 | 当前 PR-1 / PR-2 | 以 KEP 正文及 #4217 校准范围；推荐消费一致性修复和变体生命周期两个 PR，前者先复现验证，无法独立成立则并入后者；测试与文档随功能交付。 | 本文更新；尚未运行复现/功能测试 |
| 2026-10-29 | Kubernetes 联动 | 加入容器在用 handler 回传、固定 kubelet 版本及删除请求 handler 核对；显式 CRI 验收与 kubelet 端到端验收分开记录。 | 待固定版本核对 |
| 2026-10-29 | PR-1 实现与验证 | 消费一致性修复保存为三个本地 commits；专项和当前 kubelet 工作负载验证通过，完整套件存在失败/跳过及前置条件阻塞；原节点运行时已恢复。 | `312058228`、`1aa5f0c9b`、`a6dcedca9`；第 4.3 节测试记录 |
