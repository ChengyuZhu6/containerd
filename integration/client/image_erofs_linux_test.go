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

package client

import (
	"testing"

	. "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	imagelist "github.com/containerd/containerd/v2/integration/images"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/image-spec/identity"
)

func TestImagePullUnpackEROFSWithDiffer(t *testing.T) {
	skipIfEROFSSnapshotterNotReady(t)

	imageName := imagelist.Get(imagelist.Alpine)
	ctx, cancel := testContext(t)
	defer cancel()

	client, err := newClient(t, address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.ImageService().Delete(ctx, imageName, images.SynchronousDelete()); err != nil && !errdefs.IsNotFound(err) {
		t.Fatal(err)
	}

	img, err := client.Pull(ctx, imageName,
		WithPlatformMatcher(platforms.Default()),
		WithPullUnpack,
		WithPullSnapshotter(erofsSnapshotterID),
	)
	if err != nil {
		t.Fatalf("pull+unpack with erofs: %v", err)
	}

	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sn := client.SnapshotService(erofsSnapshotterID)
	for _, chainID := range identity.ChainIDs(diffIDs) {
		if _, err := sn.Stat(ctx, chainID.String()); err != nil {
			t.Fatalf("snapshot %s missing in erofs: %v", chainID, err)
		}
	}
}
