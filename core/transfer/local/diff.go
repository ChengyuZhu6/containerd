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
	"io"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/log"
)

// diffSnapshot implements SnapshotSource -> LayerDestination transfer
// This creates a diff from a snapshot and writes it as a layer
func (ts *localTransferService) diffSnapshot(
	ctx context.Context,
	src transfer.SnapshotSource,
	dest transfer.LayerDestination,
	tops *transfer.Config,
) error {
	ctx, done, err := ts.withLease(ctx)
	if err != nil {
		return err
	}
	defer done(ctx)

	if tops.Progress != nil {
		tops.Progress(transfer.Progress{
			Event: "Creating diff",
		})
	}

	// Get mounts for upper (current snapshot)
	upper, err := src.GetMounts(ctx)
	if err != nil {
		return fmt.Errorf("failed to get mounts: %w", err)
	}

	// Get mounts for lower (parent snapshot)
	lower, err := src.GetParentMounts(ctx)
	if err != nil {
		return fmt.Errorf("failed to get parent mounts: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"upper_mounts": len(upper),
		"lower_mounts": len(lower),
	}).Debug("creating diff")

	// Get comparer for the snapshotter
	comparer := ts.getComparer(src.GetSnapshotter())
	if comparer == nil {
		return fmt.Errorf("no comparer available for snapshotter")
	}

	// Create diff
	var diffOpts []diff.Opt
	desc, err := comparer.Compare(ctx, lower, upper, diffOpts...)
	if err != nil {
		return fmt.Errorf("failed to compare: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"digest":    desc.Digest,
		"mediatype": desc.MediaType,
		"size":      desc.Size,
	}).Debug("created diff")

	if tops.Progress != nil {
		tops.Progress(transfer.Progress{
			Event: "created diff",
			Desc:  &desc,
		})
	}

	// Read diff content
	ra, err := ts.content.ReaderAt(ctx, desc)
	if err != nil {
		return fmt.Errorf("failed to read diff: %w", err)
	}
	defer ra.Close()

	// Create a reader from ReaderAt
	r := &readerAtWrapper{ra: ra}

	// Write to destination
	if err := dest.WriteLayer(ctx, desc, r); err != nil {
		return fmt.Errorf("failed to write layer: %w", err)
	}

	if tops.Progress != nil {
		tops.Progress(transfer.Progress{
			Event: "Completed diff",
			Desc:  &desc,
		})
	}

	return nil
}

// getComparer returns a comparer for the given snapshotter
func (ts *localTransferService) getComparer(sn snapshots.Snapshotter) diff.Comparer {
	// Try to get comparer from config's UnpackPlatforms
	// Note: In the real implementation, you might need a separate config for comparers
	// For now, we assume the applier can also be used as a comparer if it implements both interfaces
	for _, up := range ts.config.UnpackPlatforms {
		if up.Snapshotter == sn {
			if comparer, ok := up.Applier.(diff.Comparer); ok {
				return comparer
			}
		}
	}

	// Return nil if no comparer found
	return nil
}

// readerAtWrapper wraps a ReaderAt to provide a Reader interface
type readerAtWrapper struct {
	ra     content.ReaderAt
	offset int64
}

func (r *readerAtWrapper) Read(p []byte) (n int, err error) {
	n, err = r.ra.ReadAt(p, r.offset)
	r.offset += int64(n)
	return
}

func (r *readerAtWrapper) Close() error {
	return r.ra.Close()
}
