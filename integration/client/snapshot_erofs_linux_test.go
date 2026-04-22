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
	"context"
	"fmt"
	"testing"

	. "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/testsuite"
	"github.com/containerd/containerd/v2/plugins"
)

const erofsSnapshotterID = "erofs"

func newEROFSSnapshotter(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
	c, err := New(address)
	if err != nil {
		return nil, nil, err
	}
	return c.SnapshotService(erofsSnapshotterID), c.Close, nil
}

func skipIfEROFSSnapshotterNotReady(t *testing.T) {
	t.Helper()
	c, err := New(address)
	if err != nil {
		t.Skipf("connect containerd: %v", err)
	}
	defer c.Close()

	ctx, cancel := testContext(t)
	defer cancel()

	resp, err := c.IntrospectionService().Plugins(ctx,
		fmt.Sprintf("type==%s,id==%s", plugins.SnapshotPlugin, erofsSnapshotterID))
	if err != nil || len(resp.Plugins) == 0 {
		t.Skipf("erofs snapshotter not registered: %v", err)
	}
	if e := resp.Plugins[0].InitErr; e != nil {
		t.Skipf("erofs snapshotter not ready: %s", e.Message)
	}
}

func TestSnapshotterClientEROFS(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	skipIfEROFSSnapshotterNotReady(t)

	testsuite.SnapshotterSuite(t, erofsSnapshotterID, newEROFSSnapshotter)
}
