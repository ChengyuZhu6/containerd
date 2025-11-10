# Quick Start Guide: Unpack and Diff Operations

This guide shows you how to quickly get started with the new unpack and diff transfer operations.

## Prerequisites

```go
import (
    "context"
    
    "github.com/containerd/containerd/v2/core/content"
    "github.com/containerd/containerd/v2/core/snapshots"
    "github.com/containerd/containerd/v2/core/transfer"
    "github.com/containerd/containerd/v2/core/transfer/layer"
    "github.com/containerd/containerd/v2/core/transfer/local"
    "github.com/containerd/containerd/v2/core/transfer/snapshot"
)
```

## Setup Transfer Service

```go
// Setup transfer service with your content store and snapshotter
tc := local.TransferConfig{
    UnpackPlatforms: []unpack.Platform{
        {
            Platform:       platforms.DefaultSpec(),
            SnapshotterKey: "overlayfs",
            Snapshotter:    mySnapshotter,
            Applier:        myApplier, // Your diff.Applier implementation
        },
    },
}

ts := local.NewTransferService(contentStore, imageStore, tc)
```

## Example 1: Unpack a Layer

```go
func unpackLayer(ctx context.Context) error {
    // Get a layer descriptor from your content store
    var layerDesc ocispec.Descriptor // e.g., from an image manifest
    
    // Create layer source
    src := layer.NewStreamFromDescriptor(layerDesc, contentStore)
    
    // Create snapshot destination
    dest := snapshot.NewDestination(mySnapshotter, "my-snapshot")
    
    // Execute transfer
    return ts.Transfer(ctx, src, dest)
}
```

## Example 2: Create a Diff

```go
func createDiff(ctx context.Context) error {
    // Create snapshot source
    src := snapshot.NewSource(mySnapshotter, "my-snapshot",
        snapshot.WithSourceParent("parent-snapshot"))
    
    // Create layer destination
    dest := layer.NewDestination(contentStore)
    
    // Execute transfer
    return ts.Transfer(ctx, src, dest)
}
```

## Example 3: With Progress Tracking

```go
func unpackWithProgress(ctx context.Context, layerDesc ocispec.Descriptor) error {
    src := layer.NewStreamFromDescriptor(layerDesc, contentStore)
    dest := snapshot.NewDestination(mySnapshotter, "my-snapshot")
    
    return ts.Transfer(ctx, src, dest, 
        transfer.WithProgress(func(p transfer.Progress) {
            fmt.Printf("[%s] %d/%d\n", p.Event, p.Progress, p.Total)
        }))
}
```

## Example 4: Unpack Multiple Layers

```go
func unpackLayers(ctx context.Context, layers []ocispec.Descriptor) error {
    var parent string
    
    for i, desc := range layers {
        key := fmt.Sprintf("layer-%d", i)
        
        src := layer.NewStreamFromDescriptor(desc, contentStore)
        dest := snapshot.NewDestination(mySnapshotter, key,
            snapshot.WithParent(parent))
        
        if err := ts.Transfer(ctx, src, dest); err != nil {
            return err
        }
        
        parent = key // Use as parent for next layer
    }
    
    return nil
}
```

## Example 5: From Stream

```go
func unpackFromStream(ctx context.Context, r io.Reader) error {
    // Create layer source from stream
    src := layer.NewStream(r,
        layer.WithMediaType("application/vnd.oci.image.layer.v1.tar+gzip"))
    
    // Create snapshot destination
    dest := snapshot.NewDestination(mySnapshotter, "from-stream")
    
    // Execute transfer
    return ts.Transfer(ctx, src, dest)
}
```

## Example 6: With Custom Labels

```go
func unpackWithLabels(ctx context.Context, layerDesc ocispec.Descriptor) error {
    src := layer.NewStreamFromDescriptor(layerDesc, contentStore)
    
    dest := snapshot.NewDestination(mySnapshotter, "labeled-snapshot",
        snapshot.WithLabels(map[string]string{
            "app":     "myapp",
            "version": "1.0",
        }))
    
    return ts.Transfer(ctx, src, dest)
}
```

## Common Patterns

### Pattern 1: Build Image from Snapshots

```go
// 1. Make changes to a snapshot
// 2. Create diff
src := snapshot.NewSource(sn, "modified-snapshot",
    snapshot.WithSourceParent("base-snapshot"))
dest := layer.NewDestination(cs)
ts.Transfer(ctx, src, dest)

// 3. Use the diff as a new layer in your image
```

### Pattern 2: Extract Single Layer

```go
// Extract just one layer from an image without pulling the whole image
layerDesc := manifest.Layers[0] // Get specific layer
src := layer.NewStreamFromDescriptor(layerDesc, cs)
dest := snapshot.NewDestination(sn, "extracted-layer")
ts.Transfer(ctx, src, dest)
```

### Pattern 3: Snapshot Backup

```go
// Create a layer from a snapshot for backup
src := snapshot.NewSource(sn, "important-snapshot")
dest := layer.NewDestination(cs,
    layer.WithLabels(map[string]string{
        "backup": "true",
        "date":   time.Now().Format(time.RFC3339),
    }))
ts.Transfer(ctx, src, dest)
```

## Error Handling

```go
func robustUnpack(ctx context.Context, desc ocispec.Descriptor) error {
    src := layer.NewStreamFromDescriptor(desc, cs)
    dest := snapshot.NewDestination(sn, "my-snapshot")
    
    if err := ts.Transfer(ctx, src, dest); err != nil {
        // Check error type
        if errdefs.IsAlreadyExists(err) {
            // Snapshot already exists
            return nil
        }
        if errdefs.IsNotFound(err) {
            // Layer not found in content store
            return fmt.Errorf("layer not found: %w", err)
        }
        return fmt.Errorf("unpack failed: %w", err)
    }
    
    return nil
}
```

## Next Steps

- Read [UNPACK_DIFF.md](./UNPACK_DIFF.md) for detailed documentation
- Check [examples/unpack_diff_example.go](./examples/unpack_diff_example.go) for more examples
- Review [transfer.md](../../docs/transfer.md) for complete transfer service documentation

## Troubleshooting

### "no applier available for snapshotter"

Make sure your TransferConfig includes the snapshotter with an applier:

```go
tc := local.TransferConfig{
    UnpackPlatforms: []unpack.Platform{
        {
            SnapshotterKey: "overlayfs",
            Snapshotter:    mySnapshotter,
            Applier:        myApplier, // Required!
        },
    },
}
```

### "no comparer available for snapshotter"

For diff operations, the applier must also implement `diff.Comparer`:

```go
type MyDiffer struct {
    // implements both diff.Applier and diff.Comparer
}
```

### Progress not working

Make sure to pass the progress function:

```go
ts.Transfer(ctx, src, dest, transfer.WithProgress(myProgressFunc))
```
