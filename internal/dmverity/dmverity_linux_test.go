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
	"bytes"
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
	if _, err := exec.LookPath("veritysetup"); err != nil {
		t.Skip("veritysetup not found, skipping test")
	}

	t.Run("SeparateMode", testDmVeritySeparate)
	t.Run("CombinedMode", testDmVerityCombined)
}

func testDmVeritySeparate(t *testing.T) {
	f := setupTest(t, "separate")
	defer f.cleanup()

	// Generate hash tree
	header, rootHash, err := GenerateHashTree(f.dataFile.Name(), f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.config.RootDigest = rootHash

	// Write hash file
	if err := os.Truncate(f.hashFile.Name(), 4096); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.hashFile.Name(), header, 0644); err != nil {
		t.Fatal(err)
	}

	// Enable and verify device
	if err := Enable(f.deviceName, f.dataLoop, f.hashLoop, f.config); err != nil {
		t.Fatal(err)
	}

	f.verifyDeviceContent()
	f.testCorruption()
}

func testDmVerityCombined(t *testing.T) {
	f := setupTest(t, "combined")
	defer f.cleanup()

	// Generate hash tree
	header, rootHash, err := GenerateHashTree(f.dataFile.Name(), f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.config.RootDigest = rootHash

	// Write combined data
	combinedData := append(f.testData, header...)
	if err := os.WriteFile(f.dataFile.Name(), combinedData, 0644); err != nil {
		t.Fatal(err)
	}

	// Enable and verify device
	if err := Enable(f.deviceName, f.dataLoop, f.dataLoop, f.config); err != nil {
		t.Fatal(err)
	}

	f.verifyDeviceContent()
	f.testCorruption()
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

type testFixture struct {
	t          *testing.T
	dataFile   *os.File
	hashFile   *os.File
	dataLoop   string
	hashLoop   string
	deviceName string
	config     VerityConfig
	testData   []byte
}

func setupTest(t *testing.T, mode string) *testFixture {
	f := &testFixture{t: t}

	// Create test files
	var err error
	f.dataFile, err = os.CreateTemp("", "verity-data-*")
	if err != nil {
		t.Fatal(err)
	}

	if mode == "separate" {
		f.hashFile, err = os.CreateTemp("", "verity-hash-*")
		if err != nil {
			t.Fatal(err)
		}
	}

	// Prepare test data with full block size
	f.testData = make([]byte, DefaultBlockSize)
	copy(f.testData, []byte("test data"))
	// Fill the rest of the block with zeros
	for i := len("test data"); i < len(f.testData); i++ {
		f.testData[i] = 0
	}

	// Write test data and truncate to full block size
	if _, err := f.dataFile.Write(f.testData); err != nil {
		t.Fatal(err)
	}
	if err := f.dataFile.Sync(); err != nil {
		t.Fatal(err)
	}

	// Setup loop devices
	f.dataLoop, err = setupLoopDevice(f.dataFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	if mode == "separate" {
		f.hashLoop, err = setupLoopDevice(f.hashFile.Name())
		if err != nil {
			t.Fatal(err)
		}
	}

	// Generate random salt
	salt := make([]byte, DefaultSaltSize)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}

	// Configure verity
	f.config = VerityConfig{
		Version:       1,
		HashAlgorithm: HashAlgoSHA256,
		DataBlockSize: DefaultBlockSize,
		HashBlockSize: DefaultBlockSize,
		DataBlocks:    1,
		Salt:          salt,
	}

	if mode == "combined" {
		f.config.HashOffset = DefaultBlockSize
	}

	// Generate device name
	f.deviceName, err = generateDeviceName("verity-test")
	if err != nil {
		t.Fatal(err)
	}

	return f
}

func (f *testFixture) cleanup() {
	if f.dataFile != nil {
		os.Remove(f.dataFile.Name())
	}
	if f.hashFile != nil {
		os.Remove(f.hashFile.Name())
	}
	if f.dataLoop != "" {
		cleanupLoopDevice(f.dataLoop)
	}
	if f.hashLoop != "" {
		cleanupLoopDevice(f.hashLoop)
	}
	if f.deviceName != "" {
		RemoveVerityDevice(f.deviceName)
	}
}

func (f *testFixture) verifyDeviceContent() {
	verityDevice := "/dev/mapper/" + f.deviceName
	verityFile, err := os.OpenFile(verityDevice, os.O_RDONLY, 0)
	if err != nil {
		f.t.Fatalf("failed to open verity device: %v", err)
	}
	defer verityFile.Close()

	readData := make([]byte, DefaultBlockSize)
	if _, err := verityFile.Read(readData); err != nil {
		f.t.Fatalf("failed to read from verity device: %v", err)
	}

	if !bytes.Equal(readData, f.testData) {
		f.t.Errorf("content mismatch:\ngot:  %x\nwant: %x", readData, f.testData)
	}
}

func (f *testFixture) testCorruption() {
	// Close and remove current device
	if err := RemoveVerityDevice(f.deviceName); err != nil {
		f.t.Fatalf("failed to remove verity device: %v", err)
	}

	// Corrupt data and verify detection
	if err := corruptFile(f.dataFile.Name()); err != nil {
		f.t.Fatalf("failed to corrupt file: %v", err)
	}

	// Setup new loop device for corrupted data
	var err error
	f.dataLoop, err = setupLoopDevice(f.dataFile.Name())
	if err != nil {
		f.t.Fatalf("failed to setup loop device: %v", err)
	}

	// Try to create device with corrupted data
	corruptedName := f.deviceName + "-corrupted"
	hashDev := f.hashLoop
	if hashDev == "" {
		hashDev = f.dataLoop
	}

	err = Enable(corruptedName, f.dataLoop, hashDev, f.config)
	if err != nil {
		f.t.Fatalf("failed to enable verity device: %v", err)
	}
	defer RemoveVerityDevice(corruptedName)

	// Reading from corrupted device should fail
	device := "/dev/mapper/" + corruptedName
	file, err := os.OpenFile(device, os.O_RDONLY, 0)
	if err != nil {
		f.t.Fatalf("failed to open corrupted device: %v", err)
	}
	defer file.Close()

	data := make([]byte, DefaultBlockSize)
	if _, err := file.Read(data); err == nil {
		f.t.Error("expected error when reading from corrupted device")
	} else {
		f.t.Logf("Corruption detected as expected: %v", err)
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

	_, err = f.WriteAt([]byte{0xFF}, 0)
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
