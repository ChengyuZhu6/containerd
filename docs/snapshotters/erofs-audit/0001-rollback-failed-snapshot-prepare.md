# EROFS snapshot creation is not atomic when mount generation fails

## Status

Fixed locally by `snapshots/erofs: roll back failed snapshot creation`.

## Problem

`Prepare` and `View` committed the new snapshot to `metadata.db` before
constructing its mount list. Mount construction can fail, for example when a
parent layer blob or required dm-verity sidecar is missing. The error cleanup
removed the new snapshot directory but could not roll back the already
committed metadata transaction.

The failed key was therefore left in an inconsistent state: `Stat` found it,
its snapshot directory did not exist, and retrying the operation with the same
key failed with `AlreadyExists`.

## Reproduction

The regression test creates a committed parent whose `layer.erofs` is missing,
then prepares a child:

```console
$ go test ./plugins/snapshots/erofs \
    -run TestCreateSnapshotRollsBackWhenMountsFail -count=1
--- FAIL: TestCreateSnapshotRollsBackWhenMountsFail
    erofs_test.go:54: An error is expected but got nil.
```

The assertion fails on the unpatched code because `Stat("child")` succeeds
after `Prepare("child", "committed-parent")` has returned an error.

## Root cause

`createSnapshot` called `s.mounts` only after `MetaStore.WithTransaction` had
returned successfully. Its deferred cleanup covered filesystem paths only.

## Resolution

Construct the mount list before returning from the metadata transaction. A
mount-generation error now rolls back the transaction, while the existing
deferred cleanup removes the renamed snapshot directory.

## Community overlap

No open containerd EROFS issue or pull request found on 2026-09-10 covers this
transactional failure mode.
