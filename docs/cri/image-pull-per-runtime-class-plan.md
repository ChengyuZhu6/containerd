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

## Next steps

1. Review the compatibility contract and the complete feature diff; keep PR-1 separate.
2. Resolve full-suite environment blockers and record all skips and failures, including any unclassified integration failures.
3. Validate a supported kubelet version's pull/status/list/container tuples and removal with the feature enabled.
4. Obtain independent maintainer review before opening a PR. All implementation and review communication require human review.
