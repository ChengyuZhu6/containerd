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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/testsuite"
	"github.com/containerd/containerd/v2/pkg/testutil"
)

func newSnapshotter(t *testing.T) func(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
	_, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		t.Skipf("could not find mkfs.erofs: %v", err)
	}

	if !findErofs() {
		t.Skip("check for erofs kernel support failed, skipping test")
	}
	return func(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
		var opts []Opt

		snapshotter, err := NewSnapshotter(root, opts...)
		if err != nil {
			return nil, nil, err
		}

		return snapshotter, func() error { return snapshotter.Close() }, nil
	}
}

func TestErofs(t *testing.T) {
	testutil.RequiresRoot(t)
	testsuite.SnapshotterSuite(t, "erofs", newSnapshotter(t))
}

func TestErofsVerity(t *testing.T) {
	testutil.RequiresRoot(t)

	// Check if veritysetup is available
	_, err := exec.LookPath("veritysetup")
	if err != nil {
		t.Skipf("could not find veritysetup: %v", err)
	}

	ctx := context.Background()
	root := t.TempDir()

	// Create snapshotter with verity enabled
	opts := []Opt{
		WithVerity(),
		WithVerityHashAlgorithm("sha256"),
	}
	snapshotter, err := NewSnapshotter(root, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshotter.Close()

	// Prepare a snapshot
	key := "test-verity"
	mounts, err := snapshotter.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}

	// Write some test data
	dir := t.TempDir()
	if err := mount.All(mounts, dir); err != nil {
		t.Fatal(err)
	}
	defer mount.UnmountAll(dir, 0)

	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("test data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Commit the snapshot
	name := "test-verity-commit"
	if err := snapshotter.Commit(ctx, name, key); err != nil {
		t.Fatal(err)
	}

	// Verify that verity metadata files were created
	snap := filepath.Join(root, "snapshots", name)
	files := []string{
		filepath.Join(snap, "layer.erofs"),             // EROFS image
		filepath.Join(snap, "layer.erofs.verity"),      // Verity hash tree
		filepath.Join(snap, "layer.erofs.verity.meta"), // Verity metadata
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected verity file %s to exist: %v", f, err)
		}
	}

	// Verify the metadata content
	meta, err := os.ReadFile(filepath.Join(snap, "layer.erofs.verity.meta"))
	if err != nil {
		t.Fatal(err)
	}

	var verityMeta struct {
		HashAlgorithm string `json:"hash_algorithm"`
		RootHash      string `json:"root_hash"`
	}
	if err := json.Unmarshal(meta, &verityMeta); err != nil {
		t.Fatal(err)
	}

	if verityMeta.HashAlgorithm != "sha256" {
		t.Errorf("expected hash algorithm sha256, got %s", verityMeta.HashAlgorithm)
	}
	if verityMeta.RootHash == "" {
		t.Error("expected root hash to be present")
	}

	// Test mounting with verity
	mounts, err = snapshotter.View(ctx, "test-verity-view", name)
	if err != nil {
		t.Fatal(err)
	}

	viewDir := t.TempDir()
	if err := mount.All(mounts, viewDir); err != nil {
		t.Fatal(err)
	}
	defer mount.UnmountAll(viewDir, 0)

	// Verify the content
	content, err := os.ReadFile(filepath.Join(viewDir, "test.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "test data" {
		t.Errorf("expected content 'test data', got '%s'", string(content))
	}
}

// Test that verity can be disabled
func TestErofsVerityDisabled(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.Background()
	root := t.TempDir()

	// Create snapshotter with verity disabled (default)
	snapshotter, err := NewSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshotter.Close()

	// Create and commit a snapshot
	key := "test-no-verity"
	mounts, err := snapshotter.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := mount.All(mounts, dir); err != nil {
		t.Fatal(err)
	}
	defer mount.UnmountAll(dir, 0)

	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("test data"), 0644); err != nil {
		t.Fatal(err)
	}

	name := "test-no-verity-commit"
	if err := snapshotter.Commit(ctx, name, key); err != nil {
		t.Fatal(err)
	}

	// Verify that verity files were not created
	snap := filepath.Join(root, "snapshots", name)
	verityFiles := []string{
		filepath.Join(snap, "layer.erofs.verity"),
		filepath.Join(snap, "layer.erofs.verity.meta"),
	}
	for _, f := range verityFiles {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("verity file %s should not exist", f)
		} else if !os.IsNotExist(err) {
			t.Errorf("unexpected error checking verity file: %v", err)
		}
	}
}

// Test error cases
func TestErofsVerityErrors(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.Background()
	root := t.TempDir()

	// Test invalid hash algorithm
	opts := []Opt{
		WithVerity(),
		WithVerityHashAlgorithm("invalid"),
	}
	snapshotter, err := NewSnapshotter(root, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshotter.Close()

	key := "test-invalid-algo"
	mounts, err := snapshotter.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := mount.All(mounts, dir); err != nil {
		t.Fatal(err)
	}
	defer mount.UnmountAll(dir, 0)

	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("test data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Commit should fail due to invalid hash algorithm
	if err := snapshotter.Commit(ctx, "test-invalid-commit", key); err == nil {
		t.Error("expected error with invalid hash algorithm")
	}
}
