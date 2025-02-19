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
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"io"
	"os"
)

// BlockDeviceSize returns size of block device in bytes
func BlockDeviceSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("failed to seek on %q: %w", path, err)
	}
	return size, nil
}

// GenerateHashTree generates the hash tree for the given data device
func GenerateHashTree(dataDevice string, config VerityConfig) ([]byte, []byte, error) {
	// Open data device
	data, err := os.Open(dataDevice)
	if err != nil {
		return nil, nil, err
	}
	defer data.Close()

	// Calculate number of blocks
	dataSize, err := BlockDeviceSize(dataDevice)
	if err != nil {
		return nil, nil, err
	}
	numBlocks := uint64(dataSize) / uint64(config.DataBlockSize)

	// Initialize hash function
	var hasher hash.Hash
	switch config.HashAlgorithm {
	case HashAlgoSHA256:
		hasher = sha256.New()
	case HashAlgoSHA512:
		hasher = sha512.New()
	default:
		return nil, nil, fmt.Errorf("unsupported hash algorithm")
	}

	// Generate hash tree
	hashTree := make([]byte, 0)
	rootHash := make([]byte, hasher.Size())

	// Read and hash each data block
	block := make([]byte, config.DataBlockSize)
	for i := uint64(0); i < numBlocks; i++ {
		_, err := io.ReadFull(data, block)
		if err != nil {
			return nil, nil, err
		}

		hasher.Reset()
		hasher.Write(config.Salt)
		hasher.Write(block)
		hashTree = append(hashTree, hasher.Sum(nil)...)
	}

	// Calculate root hash
	hasher.Reset()
	hasher.Write(config.Salt)
	hasher.Write(hashTree)
	rootHash = hasher.Sum(nil)

	return hashTree, rootHash, nil
}
