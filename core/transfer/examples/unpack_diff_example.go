/*
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
*/

package examples

import (
	"context"
	"fmt"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/core/transfer/layer"
	"github.com/containerd/containerd/v2/core/transfer/snapshot"
	"github.com/containerd/log"
)

// UnpackLayerExample demonstrates how to unpack a layer into a snapshot
func UnpackLayerExample(
	ctx context.Context,
	ts transfer.Transferrer,
	cs content.Store,
	sn snapshots.Snapshotter,
	layerDesc ocispec.Descriptor,
) error {
	log.G(ctx).Info("Starting layer unpack example")

	// Create layer source from descriptor
	layerSrc := layer.NewStreamFromDescriptor(layerDesc, cs)

	// Create snapshot destination
	snapshotKey := fmt.Sprintf("unpacked-%s", layerDesc.Digest.Encoded()[:12])
	snapDest := snapshot.NewDestination(sn, snapshotKey,
		snapshot.WithLabels(map[string]string{
			"containerd.io/snapshot.ref": layerDesc.Digest.String(),
		}))

	// Execute transfer with progress tracking
	err := ts.Transfer(ctx, layerSrc, snapDest,
		transfer.WithProgress(func(p transfer.Progress) {
			switch p.Event {
			case "Unpacking layer":
				log.G(ctx).Info("Starting unpack operation")
			case "unpacking layer":
				if p.Desc != nil {
					log.G(ctx).WithFields(log.Fields{
						"digest": p.Desc.Digest,
						"size":   p.Desc.Size,
					}).Info("Unpacking layer")
				}
			case "extracting":
				if p.Total > 0 {
					percent := float64(p.Progress) / float64(p.Total) * 100
					log.G(ctx).WithFields(log.Fields{
						"progress": p.Progress,
						"total":    p.Total,
						"percent":  fmt.Sprintf("%.2f%%", percent),
					}).Debug("Extracting layer")
				}
			case "Completed unpack":
				log.G(ctx).Info("Unpack completed successfully")
			}
		}))

	if err != nil {
		return fmt.Errorf("failed to unpack layer: %w", err)
	}

	log.G(ctx).WithField("snapshot", snapshotKey).Info("Layer unpacked to snapshot")
	return nil
}

// CreateDiffExample demonstrates how to create a diff from a snapshot
func CreateDiffExample(
	ctx context.Context,
	ts transfer.Transferrer,
	cs content.Store,
	sn snapshots.Snapshotter,
	snapshotKey string,
	parentKey string,
) (ocispec.Descriptor, error) {
	log.G(ctx).Info("Starting diff creation example")

	// Create snapshot source
	snapSrc := snapshot.NewSource(sn, snapshotKey,
		snapshot.WithSourceParent(parentKey))

	// Create layer destination
	layerDest := layer.NewDestination(cs,
		layer.WithLabels(map[string]string{
			"containerd.io/diff.source": snapshotKey,
		}))

	var resultDesc ocispec.Descriptor

	// Execute transfer with progress tracking
	err := ts.Transfer(ctx, snapSrc, layerDest,
		transfer.WithProgress(func(p transfer.Progress) {
			switch p.Event {
			case "Creating diff":
				log.G(ctx).Info("Starting diff creation")
			case "created diff":
				if p.Desc != nil {
					resultDesc = *p.Desc
					log.G(ctx).WithFields(log.Fields{
						"digest":    p.Desc.Digest,
						"size":      p.Desc.Size,
						"mediatype": p.Desc.MediaType,
					}).Info("Diff created")
				}
			case "Completed diff":
				log.G(ctx).Info("Diff creation completed successfully")
			}
		}))

	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to create diff: %w", err)
	}

	log.G(ctx).WithField("digest", resultDesc.Digest).Info("Diff layer created")
	return resultDesc, nil
}

// RoundTripExample demonstrates unpacking a layer and then creating a diff from it
func RoundTripExample(
	ctx context.Context,
	ts transfer.Transferrer,
	cs content.Store,
	sn snapshots.Snapshotter,
	layerDesc ocispec.Descriptor,
) error {
	log.G(ctx).Info("Starting round-trip example")

	// Step 1: Unpack layer to snapshot
	snapshotKey := fmt.Sprintf("roundtrip-%s", layerDesc.Digest.Encoded()[:12])
	if err := UnpackLayerExample(ctx, ts, cs, sn, layerDesc); err != nil {
		return fmt.Errorf("unpack failed: %w", err)
	}

	// Step 2: Create diff from snapshot
	diffDesc, err := CreateDiffExample(ctx, ts, cs, sn, snapshotKey, "")
	if err != nil {
		return fmt.Errorf("diff creation failed: %w", err)
	}

	// Step 3: Compare original and diff descriptors
	log.G(ctx).WithFields(log.Fields{
		"original_digest": layerDesc.Digest,
		"original_size":   layerDesc.Size,
		"diff_digest":     diffDesc.Digest,
		"diff_size":       diffDesc.Size,
	}).Info("Round-trip completed")

	// Note: The digests may differ due to compression or metadata differences
	// but the content should be equivalent

	return nil
}

// ChainedUnpackExample demonstrates unpacking multiple layers in sequence
func ChainedUnpackExample(
	ctx context.Context,
	ts transfer.Transferrer,
	cs content.Store,
	sn snapshots.Snapshotter,
	layers []ocispec.Descriptor,
) error {
	log.G(ctx).WithField("layers", len(layers)).Info("Starting chained unpack example")

	var parentKey string

	for i, layerDesc := range layers {
		log.G(ctx).WithFields(log.Fields{
			"layer": i + 1,
			"total": len(layers),
			"digest": layerDesc.Digest,
		}).Info("Unpacking layer")

		// Create layer source
		layerSrc := layer.NewStreamFromDescriptor(layerDesc, cs)

		// Create snapshot destination with parent
		snapshotKey := fmt.Sprintf("layer-%d-%s", i, layerDesc.Digest.Encoded()[:12])
		snapDest := snapshot.NewDestination(sn, snapshotKey,
			snapshot.WithParent(parentKey))

		// Execute transfer
		if err := ts.Transfer(ctx, layerSrc, snapDest); err != nil {
			return fmt.Errorf("failed to unpack layer %d: %w", i, err)
		}

		// Use this snapshot as parent for next layer
		parentKey = snapshotKey

		log.G(ctx).WithField("snapshot", snapshotKey).Info("Layer unpacked")
	}

	log.G(ctx).WithField("final_snapshot", parentKey).Info("All layers unpacked")
	return nil
}
