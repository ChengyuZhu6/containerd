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
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"log"
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

	// Read and hash each data block
	block := make([]byte, config.DataBlockSize)
	var hashTree []byte
	var dataBlocks uint64 = 0

	log.Printf("Generating hash tree:")
	log.Printf("Data block size: %d", config.DataBlockSize)
	log.Printf("Salt: %x", config.Salt)

	// 第一层：计算所有数据块的hash
	for i := uint64(0); i < config.DataBlocks; i++ {
		n, err := io.ReadFull(data, block)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, nil, err
		}
		if n == 0 {
			break
		}
		dataBlocks++

		// Calculate current block hash
		hasher.Reset()
		hasher.Write(config.Salt)
		hasher.Write(block[:n])
		blockHash := hasher.Sum(nil)

		log.Printf("Block %d hash: %x", i, blockHash)
		hashTree = append(hashTree, blockHash...)
	}

	// 如果没有数据块，使用空块
	if len(hashTree) == 0 {
		hasher.Reset()
		hasher.Write(config.Salt)
		hasher.Write(make([]byte, config.DataBlockSize))
		hashTree = hasher.Sum(nil)
		dataBlocks = 1
	}

	// 计算root hash：对所有block hash再次hash
	hasher.Reset()
	hasher.Write(config.Salt)
	hasher.Write(hashTree)
	rootHash := hasher.Sum(nil)

	log.Printf("Hash tree: %x", hashTree)
	log.Printf("Root hash: %x", rootHash)

	// 创建verity header (4096字节)
	output := make([]byte, 4096)

	// 1. Magic number
	copy(output[0:8], []byte("verity\000\000"))

	// 2. Version and hash type
	binary.LittleEndian.PutUint32(output[8:], 1)  // version
	binary.LittleEndian.PutUint32(output[12:], 1) // hash type

	// 3. Generate UUID
	uuid := make([]byte, 16)
	if _, err := rand.Read(uuid); err != nil {
		return nil, nil, err
	}
	copy(output[16:32], uuid)

	// 4. Algorithm name
	copy(output[32:], []byte("sha256\000"))

	// 5. Block sizes and counts
	binary.LittleEndian.PutUint32(output[64:], config.DataBlockSize)
	binary.LittleEndian.PutUint32(output[68:], config.HashBlockSize)
	binary.LittleEndian.PutUint32(output[72:], uint32(dataBlocks)) // actual data blocks
	binary.LittleEndian.PutUint32(output[76:], 0)                  // reserved
	binary.LittleEndian.PutUint32(output[80:], 32)                 // hash size (sha256 = 32 bytes)

	// 6. Hash size and salt at offset 0x50
	binary.LittleEndian.PutUint32(output[0x50:], 32) // hash size (32 bytes for sha256)
	binary.LittleEndian.PutUint32(output[0x54:], 0)  // 4 bytes of zeros
	copy(output[0x58:], config.Salt)                 // then salt

	// 返回header和root hash
	return output, rootHash, nil
}
