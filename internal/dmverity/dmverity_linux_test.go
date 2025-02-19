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
	"strings"
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

	// Write test data and truncate to full block size
	if _, err := dataFile.Write(testData); err != nil {
		t.Fatal(err)
	}
	if err := dataFile.Sync(); err != nil {
		t.Fatal(err)
	}

	// Create loop devices
	dataLoop, err := setupLoopDevice(dataFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupLoopDevice(dataLoop)

	hashLoop, err := setupLoopDevice(hashFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupLoopDevice(hashLoop)

	// Configure verity
	config := VerityConfig{
		Version:       1,
		HashAlgorithm: HashAlgoSHA256,
		DataBlockSize: DefaultBlockSize,
		HashBlockSize: DefaultBlockSize,
		DataBlocks:    1,
		Salt:          make([]byte, DefaultSaltSize),
	}

	// Generate hash tree and get root hash
	hashTree, rootHash, err := GenerateHashTree(dataFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}
	config.RootDigest = rootHash

	// Write hash tree
	if err := os.WriteFile(hashFile.Name(), hashTree, 0644); err != nil {
		t.Fatal(err)
	}

	// Create a verity device
	deviceName, err := generateDeviceName("verity-test")
	if err != nil {
		t.Fatal(err)
	}

	// Debug output
	t.Logf("Creating verity device with name: %s", deviceName)
	t.Logf("Data device: %s", dataLoop)
	t.Logf("Hash device: %s", hashLoop)

	// Enable verity device
	err = Enable(deviceName, dataLoop, hashLoop, config)
	if err != nil {
		t.Fatal(err)
	}
	defer RemoveVerityDevice(deviceName)

	// Verify the device was created
	if _, err := os.Stat("/dev/mapper/" + deviceName); err != nil {
		t.Errorf("verity device not found: %v", err)
	}

	// Test corruption detection
	if err := RemoveVerityDevice(deviceName); err != nil {
		t.Fatal(err)
	}

	// Detach loop devices
	if err := cleanupLoopDevice(dataLoop); err != nil {
		t.Fatal(err)
	}
	if err := cleanupLoopDevice(hashLoop); err != nil {
		t.Fatal(err)
	}

	// Corrupt the data
	if err := corruptFile(dataFile.Name()); err != nil {
		t.Fatal(err)
	}

	// Create new loop devices with corrupted data
	dataLoop, err = setupLoopDevice(dataFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupLoopDevice(dataLoop)

	hashLoop, err = setupLoopDevice(hashFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupLoopDevice(hashLoop)

	hashTree, rootHash, err = GenerateHashTree(dataFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Got corrupted root hash: %x", rootHash)

	// Try to create device with corrupted data
	if err := Enable(deviceName, dataLoop, hashLoop, config); err == nil {
		RemoveVerityDevice(deviceName)
		t.Fatal("expected error when creating device with corrupted data")
	} else {
		t.Logf("Corruption detected as expected: %v", err)
	}
}

func testDmVerityCombined(t *testing.T) {
	// Create test file with extra space for hash tree
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

	// Get data size for hash offset
	dataSize := int64(len(testData))

	// Generate hash tree first to know its size
	config := VerityConfig{
		Version:       1,
		HashAlgorithm: HashAlgoSHA256,
		DataBlockSize: DefaultBlockSize,
		HashBlockSize: DefaultBlockSize,
		DataBlocks:    1,
		Salt:          make([]byte, DefaultSaltSize),
		HashOffset:    dataSize,
	}

	hashTree, rootHash, err := GenerateHashTree(dataFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}

	// Write combined data and hash tree
	combinedData := append(testData, hashTree...)
	if err := os.WriteFile(dataFile.Name(), combinedData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create loop device after writing all data
	dataLoop, err := setupLoopDevice(dataFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupLoopDevice(dataLoop)

	// Create a verity device
	deviceName, err := generateDeviceName("verity-combined")
	if err != nil {
		t.Fatal(err)
	}

	// Debug output
	t.Logf("Creating combined verity device with name: %s", deviceName)
	t.Logf("Data+hash device: %s", dataFile.Name())
	t.Logf("Hash offset: %d", dataSize)

	// Enable verity device
	err = Enable(deviceName, dataLoop, dataLoop, config)
	if err != nil {
		t.Fatal(err)
	}
	defer RemoveVerityDevice(deviceName)

	// Verify the device was created
	if _, err := os.Stat("/dev/mapper/" + deviceName); err != nil {
		t.Errorf("verity device not found: %v", err)
	}

	// Test corruption detection
	if err := RemoveVerityDevice(deviceName); err != nil {
		t.Fatal(err)
	}

	// Detach loop device
	if err := cleanupLoopDevice(dataLoop); err != nil {
		t.Fatal(err)
	}

	// Corrupt the data
	if err := corruptFile(dataFile.Name()); err != nil {
		t.Fatal(err)
	}

	// Create new loop device with corrupted data
	dataLoop, err = setupLoopDevice(dataFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupLoopDevice(dataLoop)

	hashTree, rootHash, err = GenerateHashTree(dataFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Got corrupted root hash: %x", rootHash)

	// Try to create device with corrupted data
	if err := Enable(deviceName, dataLoop, dataLoop, config); err == nil {
		RemoveVerityDevice(deviceName)
		t.Fatal("expected error when creating device with corrupted data")
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

func setupLoopDevice(file string) (string, error) {
	output, err := exec.Command("losetup", "--find", "--show", file).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func cleanupLoopDevice(dev string) error {
	return exec.Command("losetup", "-d", dev).Run()
}

// Add this function to generate random device names
func generateDeviceName(prefix string) (string, error) {
	// Generate 8 random bytes
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Convert to hex string
	return fmt.Sprintf("%s-%x", prefix, b), nil
}
