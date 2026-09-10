# EROFS fs-verity enforcement skips parent layers

## Status

Fixed locally by `snapshots/erofs: verify fsverity on every layer mount`.

## Problem

With `enable_fsverity=true`, the snapshotter verified fs-verity only when
returning a parentless committed layer directly. Normal multi-layer active and
view snapshots create one EROFS mount per parent in a loop, but that loop did
not verify any layer. A present but unprotected or replaced parent layer was
therefore accepted despite integrity enforcement being enabled.

The merged `fsmeta.erofs` fast path also bypassed per-layer verification and the
merged metadata file is not protected when the individual blobs are committed.

## Reproduction

Create two parent layer files without fs-verity and request mounts from a
snapshotter with enforcement enabled:

```console
$ go test ./plugins/snapshots/erofs \
    -run TestMountsRejectParentWithoutFsverity -count=1 -v
=== RUN   TestMountsRejectParentWithoutFsverity
    erofs_linux_test.go:332: An error is expected but got nil.
--- FAIL: TestMountsRejectParentWithoutFsverity
```

The failure was reproduced on Linux 7.3.0-rc2.

## Root cause

The verification call was placed in the parentless special case instead of the
shared `createErofsMount` path used by regular layers. `mountFsMeta` was also
allowed regardless of the integrity policy.

## Resolution

Move verification into `createErofsMount`, so every regular layer mount passes
through the same policy check. Disable the merged-fsmeta shortcut while
fs-verity enforcement is active; the snapshotter falls back to separately
verified layer mounts rather than trusting unprotected merged metadata.

## Community overlap

The integrity tracking issue #12081 covers broader EROFS integrity goals, but
no open pull request or issue found on 2026-09-10 reports or fixes this specific
multi-layer verification bypass.
