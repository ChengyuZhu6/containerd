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
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/internal/dmverity"
	"github.com/containerd/containerd/v2/pkg/testutil"
	"golang.org/x/sys/unix"
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

func TestErofsDMVerity(t *testing.T) {
	testutil.RequiresRoot(t)

	// Check if veritysetup is available
	_, err := exec.LookPath("veritysetup")
	if err != nil {
		t.Skip("veritysetup not found, skipping test")
	}

	t.Run("CombinedMode", func(t *testing.T) { testErofsDMVerity(t, true) })
	t.Run("SeparateMode", func(t *testing.T) { testErofsDMVerity(t, false) })
}

func testErofsDMVerity(t *testing.T, combined bool) {
	testutil.RequiresRoot(t)

	ctx := context.Background()
	root := t.TempDir()

	// Create snapshotter with verity enabled
	opts := []Opt{
		WithVerity(),
		WithVerityHashAlgorithm("sha256"),
	}
	if combined {
		opts = append(opts, WithVerityCombined())
	}
	s, err := NewSnapshotter(root, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Create and populate a snapshot
	key := "test-snapshot"
	mounts, err := s.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(root, key)
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := mount.All(mounts, target); err != nil {
		t.Fatal(err)
	}
	defer testutil.Unmount(t, target)

	testData := []byte("test data for integrity verification")
	if err := os.WriteFile(filepath.Join(target, "testfile"), testData, 0644); err != nil {
		t.Fatal(err)
	}

	// Commit the snapshot
	commitKey := "test-commit"
	if err := s.Commit(ctx, commitKey, key); err != nil {
		t.Fatal(err)
	}

	snap := s.(*snapshotter)
	var id string
	if err := snap.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, _, _, err = storage.GetInfo(ctx, commitKey)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	layerPath := snap.layerBlobPath(id)
	hashDevice := layerPath
	if !combined {
		hashDevice = layerPath + ".verity"
	}

	// Get file size for hash offset
	fi, err := os.Stat(layerPath)
	if err != nil {
		t.Fatal(err)
	}

	// Configure verity
	config := dmverity.VerityConfig{
		Version:       1,
		HashAlgorithm: dmverity.HashAlgoSHA256,
		DataBlockSize: dmverity.DefaultBlockSize,
		HashBlockSize: dmverity.DefaultBlockSize,
		Salt:          make([]byte, dmverity.DefaultSaltSize),
	}
	if combined {
		config.HashOffset = fi.Size()
	}

	// Calculate DataBlocks based on file size
	config.DataBlocks = uint64((fi.Size() + int64(config.DataBlockSize) - 1) / int64(config.DataBlockSize))

	// Generate hash tree and get root hash
	hashTree, rootHash, err := dmverity.GenerateHashTree(layerPath, config)
	if err != nil {
		t.Fatal(err)
	}
	config.RootDigest = rootHash

	// Write hash tree if in separate mode
	if !combined {
		if err := os.WriteFile(hashDevice, hashTree, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Create dm-verity device
	deviceName := fmt.Sprintf("verity-test-%d", os.Getpid())
	if err := dmverity.Enable(deviceName, layerPath, hashDevice, config); err != nil {
		t.Fatal(err)
	}
	defer dmverity.RemoveVerityDevice(deviceName)

	// Test 1: Verify normal read works
	verityDevice := "/dev/mapper/" + deviceName
	if err := verifyDeviceContent(t, verityDevice, testData); err != nil {
		t.Errorf("failed to verify original content: %v", err)
	}

	// Test 2: Corrupt the file and verify it's detected
	if err := corruptFile(layerPath); err != nil {
		t.Fatal(err)
	}

	// Get new file size after corruption
	fi, err = os.Stat(layerPath)
	if err != nil {
		t.Fatal(err)
	}

	// Update config with new size
	config.HashOffset = fi.Size()
	config.DataBlocks = uint64((fi.Size() + int64(config.DataBlockSize) - 1) / int64(config.DataBlockSize))

	// Recreate verity device with corrupted data
	dmverity.RemoveVerityDevice(deviceName)
	if err := dmverity.Enable(deviceName, layerPath, hashDevice, config); err != nil {
		t.Logf("veritysetup detected corruption as expected: %v", err)
	} else {
		t.Error("corruption was not detected")
	}
}

func verifyDeviceContent(t *testing.T, devicePath string, expected []byte) error {
	// Create temporary mount point
	mountPoint := t.TempDir()

	// Mount the device
	if err := unix.Mount(devicePath, mountPoint, "erofs", unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount failed: %w", err)
	}
	defer unix.Unmount(mountPoint, 0)

	// Read and verify content
	content, err := os.ReadFile(filepath.Join(mountPoint, "testfile"))
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	if !bytes.Equal(content, expected) {
		return fmt.Errorf("content mismatch: expected %q, got %q", expected, content)
	}

	return nil
}

func corruptFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// Corrupt a byte in the file
	info, err := f.Stat()
	if err != nil {
		return err
	}

	_, err = f.WriteAt([]byte{0xFF}, info.Size()/2)
	return err
}

func createEmptyFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	// 使用 truncate 创建指定大小的文件
	return f.Truncate(size)
}
