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

package local

import (
	"context"
	"fmt"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/log"
)

// unpackLayer implements LayerSource -> SnapshotDestination transfer
// This unpacks a layer stream into a snapshot
func (ts *localTransferService) unpackLayer(
	ctx context.Context,
	src transfer.LayerSource,
	dest transfer.SnapshotDestination,
	tops *transfer.Config,
) error {
	ctx, done, err := ts.withLease(ctx)
	if err != nil {
		return err
	}
	defer done(ctx)

	if tops.Progress != nil {
		tops.Progress(transfer.Progress{
			Event: "Unpacking layer",
		})
	}

	// Get layer data
	desc, rc, err := src.GetLayer(ctx)
	if err != nil {
		return fmt.Errorf("failed to get layer: %w", err)
	}
	defer rc.Close()

	log.G(ctx).WithFields(log.Fields{
		"digest":    desc.Digest,
		"mediatype": desc.MediaType,
		"size":      desc.Size,
	}).Debug("unpacking layer")

	if tops.Progress != nil {
		tops.Progress(transfer.Progress{
			Event: "unpacking layer",
			Desc:  &desc,
		})
	}

	// Prepare snapshot
	key := fmt.Sprintf("extract-%s", desc.Digest.String())
	mounts, err := dest.PrepareSnapshot(ctx, key, "")
	if err != nil {
		return fmt.Errorf("failed to prepare snapshot: %w", err)
	}

	// Get applier for the snapshotter
	applier := ts.getApplier(dest.GetSnapshotter())
	if applier == nil {
		return fmt.Errorf("no applier available for snapshotter")
	}

	// Apply layer to snapshot
	var applyOpts []diff.ApplyOpt
	if tops.Progress != nil {
		applyOpts = append(applyOpts, diff.WithProgress(func(d ocispec.Descriptor, state int64) {
			tops.Progress(transfer.Progress{
				Event:    "extracting",
				Progress: state,
				Total:    d.Size,
				Desc:     &d,
			})
		}))
	}

	applied, err := applier.Apply(ctx, desc, mounts, applyOpts...)
	if err != nil {
		return fmt.Errorf("failed to apply layer: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"digest": applied.Digest,
		"size":   applied.Size,
	}).Debug("applied layer")

	// Commit snapshot
	var commitOpts []snapshots.Opt
	if err := dest.CommitSnapshot(ctx, key, key, commitOpts...); err != nil {
		return fmt.Errorf("failed to commit snapshot: %w", err)
	}

	if tops.Progress != nil {
		tops.Progress(transfer.Progress{
			Event: "Completed unpack",
			Desc:  &desc,
		})
	}

	return nil
}

// getApplier returns an applier for the given snapshotter
func (ts *localTransferService) getApplier(sn snapshots.Snapshotter) diff.Applier {
	// Try to get applier from config's UnpackPlatforms
	for _, up := range ts.config.UnpackPlatforms {
		if up.Snapshotter == sn {
			return up.Applier
		}
	}

	// Return nil if no applier found
	// In a real implementation, you might want to create a default applier
	return nil
}
