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
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	// Hash algorithms
	HashAlgoSHA256 = 1
	HashAlgoSHA512 = 2

	// Default values
	DefaultBlockSize = 4096
	DefaultHashSize  = 32 // SHA256
	DefaultSaltSize  = 32
)

// VerityConfig contains the configuration for dm-verity
type VerityConfig struct {
	// Version of the dm-verity format (1 or 2)
	Version uint32
	// Hash algorithm (1 = sha256, 2 = sha512)
	HashAlgorithm uint32
	// Data block size (must be power of 2)
	DataBlockSize uint32
	// Hash block size (must be power of 2)
	HashBlockSize uint32
	// Size of the data device in blocks
	DataBlocks uint64
	// Salt (optional)
	Salt []byte
	// Hash of the root hash block
	RootDigest []byte
	// Offset of hash tree in data device (0 means separate hash device)
	HashOffset int64
}

// Enable enables dm-verity on a device using veritysetup
func Enable(dataDevice string, hashDevice string, config VerityConfig) (string, error) {
	if err := validateConfig(config); err != nil {
		return "", err
	}

	// Format veritysetup arguments
	args := []string{
		"format",
		"--hash", hashAlgoToString(config.HashAlgorithm),
		"--data-block-size", fmt.Sprintf("%d", config.DataBlockSize),
		"--hash-block-size", fmt.Sprintf("%d", config.HashBlockSize),
	}

	if len(config.Salt) > 0 {
		args = append(args, "--salt", fmt.Sprintf("%x", config.Salt))
	}

	if config.HashOffset > 0 {
		args = append(args, "--hash-offset", fmt.Sprintf("%d", config.HashOffset))
	}
	args = append(args, dataDevice, hashDevice)

	// Run veritysetup format
	cmd := exec.Command("veritysetup", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("veritysetup format failed: %v, stderr: %s", err, stderr.String())
	}

	// Parse output to get root hash
	output := stdout.String()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Root hash:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Root hash:")), nil
		}
	}

	return "", fmt.Errorf("root hash not found in veritysetup output")
}

// CreateVerityTarget creates a dm-verity target device using veritysetup
func CreateVerityTarget(name, dataDevice, hashDevice string, config VerityConfig) error {
	if err := validateConfig(config); err != nil {
		return err
	}

	// Format veritysetup arguments
	args := []string{
		"create",
		name,
		dataDevice,
		hashDevice,
		string(config.RootDigest),
	}

	if config.HashOffset > 0 {
		args = append(args, "--hash-offset", fmt.Sprintf("%d", config.HashOffset))
	}

	// Run veritysetup create
	cmd := exec.Command("veritysetup", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("veritysetup create failed: %v, stderr: %s", err, stderr.String())
	}

	return nil
}

// RemoveVerityDevice removes a dm-verity device
func RemoveVerityDevice(name string) error {
	cmd := exec.Command("veritysetup", "close", name)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("veritysetup close failed: %v, stderr: %s", err, stderr.String())
	}
	return nil
}

// IsSupported checks if dm-verity is supported
func IsSupported() (bool, error) {
	// Check if veritysetup is available
	if _, err := exec.LookPath("veritysetup"); err != nil {
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
