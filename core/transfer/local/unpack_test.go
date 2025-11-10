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
	"testing"

	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/core/transfer/layer"
	"github.com/containerd/containerd/v2/core/transfer/snapshot"
)

// TestUnpackLayer demonstrates how to use the unpack layer transfer operation
func TestUnpackLayer(t *testing.T) {
	t.Skip("This is an example test showing usage")

	// Example usage:
	// 1. Create a layer source from a descriptor in content store
	// var desc ocispec.Descriptor
	// var cs content.Store
	// layerSrc := layer.NewStreamFromDescriptor(desc, cs)

	// 2. Create a snapshot destination
	// var sn snapshots.Snapshotter
	// snapDest := snapshot.NewDestination(sn, "my-snapshot-key")

	// 3. Create transfer service
	// var ts transfer.Transferrer

	// 4. Execute transfer
	// err := ts.Transfer(ctx, layerSrc, snapDest, transfer.WithProgress(progressFunc))
}

// TestDiffSnapshot demonstrates how to use the diff snapshot transfer operation
func TestDiffSnapshot(t *testing.T) {
	t.Skip("This is an example test showing usage")

	// Example usage:
	// 1. Create a snapshot source
	// var sn snapshots.Snapshotter
	// snapSrc := snapshot.NewSource(sn, "my-snapshot-key",
	//     snapshot.WithSourceParent("parent-snapshot-key"))

	// 2. Create a layer destination
	// var cs content.Store
	// layerDest := layer.NewDestination(cs)

	// 3. Create transfer service
	// var ts transfer.Transferrer

	// 4. Execute transfer
	// err := ts.Transfer(ctx, snapSrc, layerDest, transfer.WithProgress(progressFunc))
}

// Example progress function
func exampleProgressFunc(p transfer.Progress) {
	// Handle progress events
	// log.Printf("Event: %s, Progress: %d/%d", p.Event, p.Progress, p.Total)
}

// TestUnpackLayerIntegration would be a real integration test
func TestUnpackLayerIntegration(t *testing.T) {
	t.Skip("Integration test - requires full containerd setup")

	ctx := context.Background()
	_ = ctx

	// This would require:
	// - A running containerd instance or mock
	// - Content store with layer data
	// - Snapshotter instance
	// - Proper setup and teardown

	// Example flow:
	// 1. Setup content store with a layer
	// 2. Create layer source
	// 3. Create snapshot destination
	// 4. Execute transfer
	// 5. Verify snapshot was created correctly
	// 6. Cleanup
}
