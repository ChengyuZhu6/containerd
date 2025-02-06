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

package dmverity

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/containerd/containerd/v2/pkg/testutil"
)

func TestDmVerity(t *testing.T) {
	testutil.RequiresRoot(t)

	// Check if veritysetup is available
	_, err := exec.LookPath("veritysetup")
	if err != nil {
		t.Skip("veritysetup not found, skipping test")
	}

	t.Run("SeparateMode", testDmVeritySeparate)
	t.Run("CombinedMode", testDmVerityCombined)
}

func testDmVeritySeparate(t *testing.T) {
	// Create test files
	dataFile, err := os.CreateTemp("", "verity-data-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dataFile.Name())

	hashFile, err := os.CreateTemp("", "verity-hash-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(hashFile.Name())

	// Prepare test data with full block size
	testData := make([]byte, DefaultBlockSize)
	copy(testData, []byte("test data"))

	// Write test data
	if _, err := dataFile.Write(testData); err != nil {
		t.Fatal(err)
	}
	if err := dataFile.Sync(); err != nil {
		t.Fatal(err)
	}

	// Configure verity
	config := VerityConfig{
		Version:       1,
		HashAlgorithm: HashAlgoSHA256,
		DataBlockSize: DefaultBlockSize,
		HashBlockSize: DefaultBlockSize,
		DataBlocks:    1, // One full block
		Salt:          make([]byte, DefaultSaltSize),
	}

	// Enable verity
	rootHash, err := Enable(dataFile.Name(), hashFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}

	if rootHash == "" {
		t.Error("expected non-empty root hash")
	}

	t.Logf("Got root hash: %s", rootHash)

	// Create a verity device
	deviceName := fmt.Sprintf("verity-test-%d", os.Getpid())
	config.RootDigest = []byte(rootHash)

	// Debug output
	t.Logf("Creating verity device with name: %s", deviceName)
	t.Logf("Data device: %s", dataFile.Name())
	t.Logf("Hash device: %s", hashFile.Name())

	err = CreateVerityTarget(deviceName, dataFile.Name(), hashFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer RemoveVerityDevice(deviceName)

	// Verify the device was created
	if _, err := os.Stat("/dev/mapper/" + deviceName); err != nil {
		t.Errorf("verity device not found: %v", err)
	}
}

func testDmVerityCombined(t *testing.T) {
	// Create test file
	dataFile, err := os.CreateTemp("", "verity-combined-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dataFile.Name())

	// Prepare test data with full block size
	testData := make([]byte, DefaultBlockSize)
	copy(testData, []byte("test data for combined mode"))

	// Write test data
	if _, err := dataFile.Write(testData); err != nil {
		t.Fatal(err)
	}
	if err := dataFile.Sync(); err != nil {
		t.Fatal(err)
	}

	// Get file size for hash offset
	fi, err := os.Stat(dataFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	hashOffset := fi.Size()

	// Configure verity
	config := VerityConfig{
		Version:       1,
		HashAlgorithm: HashAlgoSHA256,
		DataBlockSize: DefaultBlockSize,
		HashBlockSize: DefaultBlockSize,
		DataBlocks:    1, // One full block
		Salt:          make([]byte, DefaultSaltSize),
		HashOffset:    hashOffset,
	}

	// Enable verity
	rootHash, err := Enable(dataFile.Name(), dataFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}

	if rootHash == "" {
		t.Error("expected non-empty root hash")
	}

	t.Logf("Got root hash: %s", rootHash)

	// Create a verity device
	deviceName := fmt.Sprintf("verity-combined-%d", os.Getpid())

	// Debug output
	t.Logf("Creating combined verity device with name: %s", deviceName)
	t.Logf("Data+hash device: %s", dataFile.Name())
	t.Logf("Hash offset: %d", hashOffset)

	// Update config with root hash
	config.RootDigest = []byte(rootHash)

	err = CreateVerityTarget(deviceName, dataFile.Name(), dataFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer RemoveVerityDevice(deviceName)

	// Verify the device was created
	if _, err := os.Stat("/dev/mapper/" + deviceName); err != nil {
		t.Errorf("verity device not found: %v", err)
	}

	// Test corruption detection
	// First remove the device
	if err := RemoveVerityDevice(deviceName); err != nil {
		t.Fatal(err)
	}

	// Corrupt the data
	if err := corruptFile(dataFile.Name()); err != nil {
		t.Fatal(err)
	}

	// Try to create device with corrupted data
	err = CreateVerityTarget(deviceName, dataFile.Name(), dataFile.Name(), config)
	if err == nil {
		t.Error("expected error when creating device with corrupted data")
		RemoveVerityDevice(deviceName)
	} else {
		t.Logf("Corruption detected as expected: %v", err)
	}
}

func TestVerityConfig(t *testing.T) {
	tests := []struct {
		name        string
		config      VerityConfig
		shouldError bool
	}{
		{
			name: "ValidConfig",
			config: VerityConfig{
				Version:       1,
				HashAlgorithm: HashAlgoSHA256,
				DataBlockSize: DefaultBlockSize,
				HashBlockSize: DefaultBlockSize,
				DataBlocks:    1024,
				Salt:          make([]byte, DefaultSaltSize),
			},
			shouldError: false,
		},
		{
			name: "ValidConfigWithOffset",
			config: VerityConfig{
				Version:       1,
				HashAlgorithm: HashAlgoSHA256,
				DataBlockSize: DefaultBlockSize,
				HashBlockSize: DefaultBlockSize,
				DataBlocks:    1024,
				Salt:          make([]byte, DefaultSaltSize),
				HashOffset:    1024 * DefaultBlockSize,
			},
			shouldError: false,
		},
		{
			name: "InvalidBlockSize",
			config: VerityConfig{
				Version:       1,
				HashAlgorithm: HashAlgoSHA256,
				DataBlockSize: 1000, // Not power of 2
				HashBlockSize: DefaultBlockSize,
				DataBlocks:    1024,
			},
			shouldError: true,
		},
		{
			name: "BlockSizeTooSmall",
			config: VerityConfig{
				Version:       1,
				HashAlgorithm: HashAlgoSHA256,
				DataBlockSize: 256, // Too small
				HashBlockSize: DefaultBlockSize,
				DataBlocks:    1024,
			},
			shouldError: true,
		},
		{
			name: "BlockSizeTooLarge",
			config: VerityConfig{
				Version:       1,
				HashAlgorithm: HashAlgoSHA256,
				DataBlockSize: 2 * 1024 * 1024, // Too large
				HashBlockSize: DefaultBlockSize,
				DataBlocks:    1024,
			},
			shouldError: true,
		},
		{
			name: "InvalidHashOffset",
			config: VerityConfig{
				Version:       1,
				HashAlgorithm: HashAlgoSHA256,
				DataBlockSize: DefaultBlockSize,
				HashBlockSize: DefaultBlockSize,
				DataBlocks:    1024,
				HashOffset:    1000, // Doesn't match DataBlocks * DataBlockSize
			},
			shouldError: true,
		},
		{
			name: "UnsupportedVersion",
			config: VerityConfig{
				Version:       3,
				HashAlgorithm: HashAlgoSHA256,
				DataBlockSize: DefaultBlockSize,
				HashBlockSize: DefaultBlockSize,
				DataBlocks:    1024,
			},
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateConfig(tt.config); (err != nil) != tt.shouldError {
				t.Errorf("validateConfig() error = %v, shouldError = %v", err, tt.shouldError)
			}
		})
	}
}

// Helper functions

func createTestFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return err
	}

	_, err = f.Write(data)
	return err
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

func corruptFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// Corrupt a byte in the middle of the file
	info, err := f.Stat()
	if err != nil {
		return err
	}

	_, err = f.WriteAt([]byte{0xFF}, info.Size()/2)
	return err
}

func removeTarget(name string) error {
	// Use dmsetup remove
	return nil // TODO: Implement actual removal using device mapper
}
