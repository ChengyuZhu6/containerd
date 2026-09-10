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

package erofs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
)

func TestCreateSnapshotRollsBackWhenMountsFail(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "snapshots"), 0700))

	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ms.Close()) })
	s := &snapshotter{root: root, ms: ms}

	var parentID string
	require.NoError(t, ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		snap, err := storage.CreateSnapshot(ctx, snapshots.KindActive, "parent", "")
		if err != nil {
			return err
		}
		parentID = snap.ID
		_, err = storage.CommitActive(ctx, "parent", "committed-parent", snapshots.Usage{})
		return err
	}))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "snapshots", parentID, "fs"), 0700))

	_, err = s.Prepare(ctx, "child", "committed-parent")
	require.ErrorContains(t, err, "failed to find valid erofs layer blob")

	_, err = s.Stat(ctx, "child")
	require.Error(t, err)
	require.True(t, errdefs.IsNotFound(err), "failed Prepare left metadata behind: %v", err)
}
