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

# KEP-4216 implementation tracking

## Scope and decisions

- Tracking issue: [containerd/containerd#14252](https://github.com/containerd/containerd/issues/14252).
- Design: [KEP-4216](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4216-image-pull-per-runtime-class) and [enhancements#4217](https://github.com/kubernetes/enhancements/pull/4217).
- Baseline: `43aa4ea134f3e50791b8537c48a18303f0fcc775`, including #14255 and #14314.
- A PR must deliver an independently usable feature with its persistence, consumers, deletion, tests and documentation. Store-only or API-only intermediate implementations are not separate feature PRs.
- Snapshotter per RuntimeClass is a KEP non-goal. This work respects existing runtime snapshotter configuration without adding configuration capabilities.

## PR-1: selected-image platform consumption

Status: deferred on the `image-platform` branch. The commits and reproduction evidence are preserved; they are not included in this branch.

The previous Linux cross-platform checks demonstrated config/rootfs reselection during container creation, but did not run foreign-architecture binaries or a Windows Hyper-V guest workload. Ordinary same-platform kubelet tests are compatibility checks, not validation of the intended guest-platform scenario. Resume this work only with an appropriate execution environment and a confirmed independently usable fix.

Preserved commits: `312058228`, `1aa5f0c9b`, `a6dcedca9`; previous tracking documentation: `77377307b`.

## PR-2: handler image lifecycle

Branch: `feat/cri-runtime-handler-images`, created directly from the baseline without PR-1.

Confirmed compatibility contract:

- An empty handler selects the configured default handler, not a global view or global deletion.
- Legacy and externally imported ordinary image references belong to the default-handler compatibility view.
- A non-default handler needs an explicit image pull or an explicitly scoped metadata association.
- Deleting one handler's image does not delete another handler's image, even when they share a config ID, content or snapshotter.

Implementation:

- [x] Reversible metadata names for non-default handlers; ordinary names remain for the default handler and existing containerd clients.
- [x] Separate in-memory image indexes, ID-prefix lookup and pinned-reference state per handler.
- [x] Direct scoped destinations for local and transfer pulls without changing the registry source reference.
- [x] Scoped image status, filtering, list and stream responses with original tags/digests and `Spec.RuntimeHandler`.
- [x] ID anchors, image metadata events and startup recovery.
- [x] Container and sandbox scoped resolution, persisted image handler and consistent status/list information.
- [x] Legacy container recovery maps an absent image handler to the default image association.
- [x] Original registry references at snapshot annotation boundaries; internal metadata names do not leak into CRI tags or container image names.
- [x] Handler-isolated, idempotent removal with conditional target deletion and shared-resource GC protection.
- [x] Necessary unit tests and one real lifecycle integration matrix, including final content and snapshot collection.
- [x] Compatibility documentation in `docs/cri/config.md`.
- [ ] Full supported-environment CRI/Kubernetes acceptance without unresolved failures or required skips.
- [ ] Maintainer/human review and submission. No remote PR has been created.

Implementation commit: `18a6f3913`.
Integration commit: `97ae85160`; validation and documentation commit: `35e1f1632`.

## Reproduction and validation: 2026-10-29

Current node: Linux amd64, kubelet/Kubernetes `v1.29.1`, runc `v1.3.3`. Builds use `no_btrfs`; btrfs is not covered.

The baseline was reproduced with real host-architecture busybox containers using both shared-native and overlayfs/native handlers. Before the fix, B incorrectly saw A's image, list collapsed the handlers, and deleting A prevented creating a new B container without pulling again. Both containers actually started and executed commands; this was not a cross-architecture creation-only test.

| Check | Result | Local evidence under `_output/pr2/` |
| --- | --- | --- |
| Client, CRI internal and plugin package tests | Passed | `packages.log` |
| Client/server/images/podsandbox/image-store race tests | Passed | `race.log` |
| `make check` | Passed, 0 issues | `make-check-final.log` |
| Real handler lifecycle | Four combinations passed: local/transfer pull and shared/distinct snapshotter; imported defaults, list/stream filters, metadata events, running-container recovery, isolated/idempotent removal, new B execution without repull, final content/snapshot GC | `lifecycle-gc.log` |
| Existing unpack/snapshot-label regression | Skipped because erofs tooling was unavailable; association setup adapted without removing fallback assertions | `lifecycle-final.log` |
| Current-node kubelet workloads | Default/native RuntimeClass init/app containers, volumes, exec/logs/copy, DNS, cross-Pod networking and daemon/kubelet recovery passed | `k8s-node-e2e.log`, `k8s-node-e2e.exit` |
| Full critest v1.35.0, without selection filters | 97 passed, 2 failed, 14 skipped; not accepted as a fully passing suite | `conformance.log`, `conformance.exit` |
| Exact baseline comparison of critest failures | Both userns/idmap and host-port specs also failed on `43aa4ea` with the same configuration | `conformance-baseline.log` |
| Broader repository CRI integration | 49 top-level tests passed, 3 failed, 22 skipped before a separate restart/failpoint case hung; interrupted with exit 143. `TestRestartContainerd` was excluded, but `TestContainerdRestartSandboxRecover` also requires the unavailable harness | `integration-suite.log`, `integration-suite.exit` |
| Upgrade compatibility | Both 2.3 and 2.4 upgrade groups passed, including container recovery, execution, image recovery and manipulation | `integration-suite.log` |

The current kubelet does not enable `RuntimeClassInImageCriApi`. Non-default node-test images were explicitly registered through CRI before starting the native RuntimeClass workload. This proves consumer compatibility, not modern kubelet handler propagation or automatic handler-aware image GC. A fixed Kubernetes version with the feature enabled is still required for KEP-wide acceptance; this is not an official Kubernetes `e2e-node` full-suite run.

The complete conformance failures concern host-port legacy iptables/NAT support and user namespace/idmap behavior. They reproduce on the exact baseline, but remain acceptance blockers; skipped specs are not counted as coverage. Broader integration additionally requires suitable loopback, Docker, failpoint/NRI and restart harness setup. Do not weaken tests or invoke global process cleanup on the active node to obtain a green result.

Node-test namespace and newly created scoped references were removed. Original containerd, shim and ctr binaries were restored byte-for-byte; configuration was unchanged and the node remained Ready. Backup files, test programs and logs are local ignored artifacts, not submitted source.

## Official Kubernetes v1.37.2 feature-gate validation

Following the earlier v1.29.1 compatibility run, the official `v1.37.2` kubelet,
kube-apiserver and kubectl were downloaded from `dl.k8s.io` and checked against their
published SHA256 files. An independently downloaded and checksum-verified etcd
`v3.6.5` supported a process-level isolated test cluster on the same node.

Production services were not replaced. The test API and kubelet ports listened on
loopback, with separate certificates, kubeconfigs, runtime root/state/socket, kubelet
root/log paths and `/pr2-feature-test` Pod cgroups. There was no scheduler or controller
manager; Pods were explicitly assigned to the test node and used host networking.
This is a targeted kubelet/CRI end-to-end test, not the Kubernetes official e2e suite.

The running kubelet's `configz` confirmed `RuntimeClassInImageCriApi: true` and
`imageMaximumGCAge: 1m`. Two RuntimeClass Pods, A and B, used the same host-architecture
busybox image and native snapshotter. Images were not pre-registered: the official
kubelet automatically queried and pulled each handler, and both Pods reached Ready.

A was deleted through the test Kubernetes API. After its container had been cleaned
up, the test waited for the unmodified five-minute kubelet image-GC cycle. A test-only
transparent CRI proxy forwarded raw protobuf payloads without rewriting them, recording
requests for evidence. The observer only queried image/container state; it did not call
`RemoveImage` or otherwise substitute for automatic GC.

| Check | Actual result |
| --- | --- |
| Automatic image status and pulls carry A/B handlers | Passed |
| A/B images share the config ID and both workloads run | Passed |
| Running B reports the corresponding image handler | Passed |
| GC identifies unused A after its container is gone | Passed: kubelet log records `runtimeHandler="a"` |
| Actual GC deletion request carries A handler | Failed: CRI `RemoveImage` has an empty handler |
| A image disappears after automatic GC | Failed: A remains queryable |
| B image, Pod identity and restart count are preserved | Passed |
| A new B Pod with `imagePullPolicy: Never` runs without repulling | Passed; total image pull requests remained two |

At `2026-10-29T14:54:44Z`, kubelet logged collection of A but sent a deletion request
without the handler. Kubelet then logged GC success even though A remained. Under the
confirmed containerd contract, an empty handler selects the default association; there
was no corresponding default image, so deletion was idempotently successful without
removing A. This is an observed integration failure, not a passing end-to-end GC result.

The exact release source explains the lost context: [GC manager](https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/kubelet/images/image_gc_manager.go#L524-L530)
passes `RuntimeHandler`, but [the runtime adapter](https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/kubelet/kuberuntime/kuberuntime_image.go#L137-L140)
constructs the CRI request using only `Image`. The downloaded kubelet was not modified
or rebuilt to mask this failure. This finding applies to the tested exact release; it
is not a claim that every Kubernetes release has the same behavior.

Local evidence: `_output/pr2/k8s-feature/feature-gc-result.json`, `cri-requests.jsonl`,
`kubelet.log`, `kubelet-configz.json`, `pods-running.json`, `pods-after-gc.json`,
`observed-before-gc.json`, `observed-after-gc.json`, exact-tag source and
`binaries.sha256`. The automatic-GC test exit code is 1. Downloads, certificates and
scripts are ignored local artifacts, not submitted source.

Test Pods, processes and empty test cgroups were cleaned up. The original production
node remained Ready, with its original services and configuration unchanged.

## Local Kubernetes fix and joint validation: 2026-10-30

The user-provided `/root/kubernetes` repository at
`0ab0f61c671640360c374426014610583fc9afb0` still omitted the runtime handler in
`kuberuntime.RemoveImage`. The extended existing deletion test failed only with the
feature enabled: expected `runc`, received an empty handler.

A separate Kubernetes branch, `fix/kubelet-image-gc-runtime-handler`, contains commit
`ad9ac018c44043df0fcfb1a08c1c65d5ee352fb2`. The production change is one line:
`RemoveImage` uses the existing `ToRuntimeAPIImageSpec` conversion, preserving the
handler only when `RuntimeClassInImageCriApi` is enabled. The existing test was extended
to inspect the actual service request for gate enabled/disabled and an empty handler;
existing deletion, error and idempotence tests remain. No gate defaults, generated
files, dependencies or GC algorithm were changed.

`pkg/kubelet/kuberuntime` and `pkg/kubelet/images` package tests, race tests and
`go vet` passed with the repository's specified Go `1.27.3`. Both kubelet and
kube-apiserver were built from the fixed commit. The local Git checkout only had an
old `v1.31.0-alpha.0` ancestor tag, so default build metadata failed the API compatibility
check. The test build explicitly used `v1.38.0-alpha.0+ad9ac018c44043`, matching the
source's `DefaultKubeBinaryVersion=1.38` and retaining the real commit. This is a
self-built development version, not an official Kubernetes release.

Joint validation used containerd commit
`69d81d801d305ed853bc2ef16bf2c2e47029ae88` in two same-node isolated API/etcd/kubelet/runtime
environments. They had different data directories, ports, Pod cgroup roots and private
mount namespaces for the kubelet's fixed device-plugin directory. A transparent CRI
proxy preserved raw requests. No image was pre-registered and the test never issued a
manual `RemoveImage`; the unmodified five-minute kubelet age-GC loop drove removal.

| Check | Gate enabled | Gate disabled |
| --- | --- | --- |
| Running configuration reports requested gate state | Passed | Passed |
| Real host-architecture Pods are automatically pulled and reach Ready | A/B handler pulls | Empty-handler/default pulls |
| Delete one Pod while retaining the other | A automatically collected; B kept | Shared default image stays in use |
| CRI deletion context | Handler `a`, then `b` on final deletion | Empty handler on final deletion |
| New Pod with `imagePullPolicy: Never` | Ready without repull | Ready without repull |
| Retained Pod identity and restart count | Unchanged | Unchanged |
| Delete remaining Pods and await automatic GC | Last B association removed | Default association removed |
| Config content and native chain snapshot collected after final GC | Passed | Passed |
| Test exit code | 0 | 0 |

Gate-enabled automatic requests carried `a` at `2026-10-30T05:46:06Z` and `b` at
`2026-10-30T05:51:06Z`. The gate-disabled request carried an empty handler at
`2026-10-30T05:51:07Z`. Final read-only resource checks confirmed both config content
and native snapshots were collected. This resolves the observed deletion-context
blocker for the tested source combination, but does not mean an unmodified official
release or all Kubernetes/CRI suites now pass.

Evidence under `_output/pr2/k8s-joint/`: `unit-before.log`, `unit-after.log`,
`remove-tests-final.log`, `race.log`, `vet.log`, exact commit files and binary checksums;
`enabled/` and `disabled/` each contain `configz.json`, `cri-requests.jsonl`, `result.json`,
`test.exit`, Pod snapshots, image observations and `final-resources.json`. Tests are
process-level targeted end-to-end checks, not the official Kubernetes e2e suite; no
scheduler/controller manager or cluster CNI was needed for these host-network Pods.

All isolated Pods, test processes, ports and empty test cgroups were cleaned up.
During initial setup, the fixed device-plugin path conflicted with the original kubelet;
the test kubelet was stopped and the original kubelet restarted to restore its socket,
then private mount isolation was applied. Production binaries/configuration were not
replaced. The production node remained Ready and kube-system container IDs/restart
counts were unchanged. Two pre-existing `sleep 900` Pods completed normally at their
15-minute interval and restarted, so the strict all-Pod identity cleanup assertion did
not pass; this was separately verified and recorded in `cleanup-result.json`.

## Next steps

1. Review the compatibility contract and the complete feature diff; keep PR-1 separate.
2. Resolve full-suite environment blockers and record all skips and failures, including any unclassified integration failures.
3. Submit the independently reviewed Kubernetes deletion fix separately from the containerd feature, then retest an official release containing it. The self-built joint test does not change the earlier official-release failure result.
4. Obtain independent maintainer review before opening either PR. All implementation and review communication require human review.
