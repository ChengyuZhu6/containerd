# Unpack and Diff Transfer Operations

This document describes the implementation of the `unpack` and `diff` transfer operations for containerd's transfer service.

## Overview

Two new transfer operations have been implemented:

1. **Unpack**: `LayerSource` → `SnapshotDestination` - Unpacks a layer stream into a snapshot
2. **Diff**: `SnapshotSource` → `LayerDestination` - Creates a diff from a snapshot and writes it as a layer

## Architecture

### Interfaces

#### LayerSource
```go
type LayerSource interface {
    GetLayer(ctx context.Context) (ocispec.Descriptor, io.ReadCloser, error)
}
```

Implementations:
- `layer.Stream` - Reads from an io.Reader or content store

#### SnapshotDestination
```go
type SnapshotDestination interface {
    PrepareSnapshot(ctx context.Context, key string, parent string) ([]mount.Mount, error)
    CommitSnapshot(ctx context.Context, name, key string, opts ...snapshots.Opt) error
    GetSnapshotter() snapshots.Snapshotter
}
```

Implementations:
- `snapshot.Destination` - Wraps a snapshotter for unpacking

#### SnapshotSource
```go
type SnapshotSource interface {
    GetMounts(ctx context.Context) ([]mount.Mount, error)
    GetParentMounts(ctx context.Context) ([]mount.Mount, error)
    GetSnapshotter() snapshots.Snapshotter
}
```

Implementations:
- `snapshot.Source` - Wraps a snapshotter for creating diffs

#### LayerDestination
```go
type LayerDestination interface {
    WriteLayer(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error
}
```

Implementations:
- `layer.Destination` - Writes to a content store

## Usage Examples

### Unpacking a Layer to a Snapshot

```go
import (
    "context"
    
    "github.com/containerd/containerd/v2/core/transfer"
    "github.com/containerd/containerd/v2/core/transfer/layer"
    "github.com/containerd/containerd/v2/core/transfer/snapshot"
)

func unpackLayer(ctx context.Context, ts transfer.Transferrer, cs content.Store, sn snapshots.Snapshotter) error {
    // Create layer source from a descriptor
    var desc ocispec.Descriptor // layer descriptor
    layerSrc := layer.NewStreamFromDescriptor(desc, cs)
    
    // Create snapshot destination
    snapDest := snapshot.NewDestination(sn, "my-snapshot-key",
        snapshot.WithParent("parent-key"),
        snapshot.WithLabels(map[string]string{
            "custom.label": "value",
        }))
    
    // Execute transfer with progress tracking
    err := ts.Transfer(ctx, layerSrc, snapDest, 
        transfer.WithProgress(func(p transfer.Progress) {
            log.Printf("Event: %s, Progress: %d/%d", p.Event, p.Progress, p.Total)
        }))
    
    return err
}
```

### Creating a Diff from a Snapshot

```go
func createDiff(ctx context.Context, ts transfer.Transferrer, cs content.Store, sn snapshots.Snapshotter) error {
    // Create snapshot source
    snapSrc := snapshot.NewSource(sn, "my-snapshot-key",
        snapshot.WithSourceParent("parent-snapshot-key"))
    
    // Create layer destination
    layerDest := layer.NewDestination(cs,
        layer.WithLabels(map[string]string{
            "layer.type": "diff",
        }))
    
    // Execute transfer
    err := ts.Transfer(ctx, snapSrc, layerDest,
        transfer.WithProgress(func(p transfer.Progress) {
            log.Printf("Event: %s", p.Event)
        }))
    
    return err
}
```

### Using with Streaming (for RPC)

```go
func unpackLayerFromStream(ctx context.Context, ts transfer.Transferrer, r io.Reader, sn snapshots.Snapshotter) error {
    // Create layer source from stream
    layerSrc := layer.NewStream(r,
        layer.WithMediaType("application/vnd.oci.image.layer.v1.tar+gzip"))
    
    // Create snapshot destination
    snapDest := snapshot.NewDestination(sn, "snapshot-from-stream")
    
    // Execute transfer
    return ts.Transfer(ctx, layerSrc, snapDest)
}
```

## Transfer Matrix

The complete transfer matrix now includes:

| Source | Destination | Operation | Status |
|--------|-------------|-----------|--------|
| Registry | Image Store | pull | ✅ Implemented (v1.7) |
| Image Store | Registry | push | ✅ Implemented (v1.7) |
| Archive | Image Store | import | ✅ Implemented (v1.7) |
| Image Store | Archive | export | ✅ Implemented (v1.7) |
| Image Store | Image Store | tag | ✅ Implemented (v1.7) |
| **LayerSource** | **SnapshotDestination** | **unpack** | ✅ **Newly Implemented** |
| **SnapshotSource** | **LayerDestination** | **diff** | ✅ **Newly Implemented** |
| Registry | Registry | mirror | ❌ Not implemented |

## Configuration

The transfer service needs to be configured with appropriate appliers and comparers for the snapshotters you want to use:

```go
import (
    "github.com/containerd/containerd/v2/core/transfer/local"
    "github.com/containerd/containerd/v2/core/unpack"
)

// Configure transfer service
tc := local.TransferConfig{
    UnpackPlatforms: []unpack.Platform{
        {
            Platform:       platforms.DefaultSpec(),
            SnapshotterKey: "overlayfs",
            Snapshotter:    overlayfsSnapshotter,
            Applier:        overlayfsApplier, // Must implement diff.Applier
        },
    },
}

ts := local.NewTransferService(contentStore, imageStore, tc)
```

## Implementation Details

### Unpack Operation Flow

1. Get layer descriptor and reader from `LayerSource`
2. Prepare snapshot using `SnapshotDestination.PrepareSnapshot()`
3. Get appropriate applier for the snapshotter
4. Apply layer to snapshot mounts using `diff.Applier.Apply()`
5. Commit snapshot using `SnapshotDestination.CommitSnapshot()`

### Diff Operation Flow

1. Get mounts for current snapshot from `SnapshotSource.GetMounts()`
2. Get mounts for parent snapshot from `SnapshotSource.GetParentMounts()`
3. Get appropriate comparer for the snapshotter
4. Create diff using `diff.Comparer.Compare()`
5. Write diff to destination using `LayerDestination.WriteLayer()`

## Progress Events

Both operations emit progress events:

### Unpack Events
- `"Unpacking layer"` - Start of operation
- `"unpacking layer"` - Layer descriptor available
- `"extracting"` - During layer extraction (with progress/total)
- `"Completed unpack"` - Operation complete

### Diff Events
- `"Creating diff"` - Start of operation
- `"created diff"` - Diff descriptor available
- `"Completed diff"` - Operation complete

## Proto Definitions

New proto messages have been added in `api/types/transfer/layer.proto`:

```protobuf
message LayerStream {
    string stream_id = 1;
    Descriptor desc = 2;
}

message SnapshotRef {
    string snapshotter = 1;
    string key = 2;
    string parent = 3;
    map<string, string> labels = 4;
}
```

## Testing

See `core/transfer/local/unpack_test.go` for usage examples and test patterns.

## Future Enhancements

1. Add support for parallel layer unpacking
2. Implement automatic applier/comparer selection based on snapshotter
3. Add validation and verification options
4. Support for incremental diffs
5. Integration with image building workflows

## Related Documentation

- [Transfer Service Documentation](../../docs/transfer.md)
- [Snapshotter Documentation](../../docs/snapshotters/)
- [Diff Service Documentation](../diff/)
