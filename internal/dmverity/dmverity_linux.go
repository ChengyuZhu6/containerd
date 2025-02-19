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
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/containerd/containerd/v2/plugins/snapshots/devmapper/dmsetup"
)

// Enable creates a dm-verity target device using dmsetup
func Enable(name, dataDevice, hashDevice string, config VerityConfig) error {
	if err := validateConfig(config); err != nil {
		return err
	}

	// Format dmsetup table arguments for verity target
	table := makeVerityTable(dataDevice, hashDevice, config)

	// Create the device with the verity target
	out, err := dmsetup.Dmsetup("create", name, "--readonly", "--table", table)
	log.Printf("dmsetup output: %s", out)
	return err
}

// RemoveVerityDevice removes a dm-verity device
func RemoveVerityDevice(name string) error {
	return dmsetup.RemoveDevice(name, dmsetup.RemoveWithRetries)
}

// makeVerityTable creates the dmsetup table string for verity target
func makeVerityTable(dataDevice, hashDevice string, config VerityConfig) string {
	// Format verity target parameters:
	// start length verity version data_dev hash_dev data_block_size hash_block_size data_blocks hash_start hash_algorithm salt root_hash
	dataBlocks := config.DataBlocks * uint64(config.DataBlockSize) / dmsetup.SectorSize

	hash_start := (config.HashOffset + int64(config.HashBlockSize) - 1) / int64(config.HashBlockSize)
	target := fmt.Sprintf("0 %d verity %d %s %s %d %d %d %d %s %x %x",
		dataBlocks,
		config.Version,
		dataDevice,
		hashDevice,
		config.DataBlockSize,
		config.HashBlockSize,
		config.DataBlocks,
		hash_start,
		hashAlgoToString(config.HashAlgorithm),
		config.RootDigest,
		config.Salt)

	// Debug output
	log.Printf("Verity table: %s", target)

	return target
}

// IsSupported checks if dm-verity is supported
func IsSupported() (bool, error) {
	// Check if dmsetup is available
	if _, err := exec.LookPath("dmsetup"); err != nil {
		return false, nil
	}

	// Check kernel module
	if _, err := os.Stat("/sys/module/dm_verity"); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// validateConfig validates the dm-verity configuration
func validateConfig(config VerityConfig) error {
	// Version must be 1 or 2
	if config.Version != 1 && config.Version != 2 {
		return fmt.Errorf("unsupported version: %d", config.Version)
	}

	// Hash algorithm must be supported
	switch config.HashAlgorithm {
	case HashAlgoSHA256, HashAlgoSHA512:
		// supported
	default:
		return fmt.Errorf("unsupported hash algorithm: %d", config.HashAlgorithm)
	}

	// Block sizes must be power of 2
	if !isPowerOfTwo(config.DataBlockSize) {
		return fmt.Errorf("data block size must be power of 2: %d", config.DataBlockSize)
	}
	if !isPowerOfTwo(config.HashBlockSize) {
		return fmt.Errorf("hash block size must be power of 2: %d", config.HashBlockSize)
	}

	// Block sizes must be at least 512 bytes and at most 1MB
	if config.DataBlockSize < 512 || config.DataBlockSize > 1024*1024 {
		return fmt.Errorf("data block size must be between 512 and 1MB: %d", config.DataBlockSize)
	}
	if config.HashBlockSize < 512 || config.HashBlockSize > 1024*1024 {
		return fmt.Errorf("hash block size must be between 512 and 1MB: %d", config.HashBlockSize)
	}

	// For combined mode, verify data size matches offset
	if config.HashOffset > 0 {
		expectedDataSize := int64(config.DataBlocks * uint64(config.DataBlockSize))
		if expectedDataSize != config.HashOffset {
			return fmt.Errorf("hash offset must match data size: expected %d, got %d", expectedDataSize, config.HashOffset)
		}
	}

	return nil
}

func isPowerOfTwo(n uint32) bool {
	return n != 0 && (n&(n-1)) == 0
}

func hashAlgoToString(algo uint32) string {
	switch algo {
	case HashAlgoSHA256:
		return "sha256"
	case HashAlgoSHA512:
		return "sha512"
	default:
		return "sha256" // default to sha256
	}
}
