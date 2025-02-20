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
	"math"
	"os"
)

type VerityHash struct {
	config VerityConfig
	// 每个哈希块可以存储的哈希值数量
	hashesPerBlock int
	// 哈希值大小
	digestSize int
}

// 创建新的 VerityHash 实例
func NewVerityHash(config VerityConfig) (*VerityHash, error) {
	if config.DataBlockSize == 0 || config.HashBlockSize == 0 {
		return nil, fmt.Errorf("invalid block size")
	}

	// 获取哈希大小
	var digestSize int
	switch config.HashAlgorithm {
	case HashAlgoSHA256:
		digestSize = sha256.Size
	case HashAlgoSHA512:
		digestSize = sha512.Size
	default:
		return nil, fmt.Errorf("unsupported hash algorithm")
	}

	// 计算每个哈希块可以存储多少个哈希值
	hashesPerBlock := int(config.HashBlockSize / uint32(digestSize))
	if hashesPerBlock == 0 {
		return nil, fmt.Errorf("hash block size too small")
	}

	return &VerityHash{
		config:         config,
		hashesPerBlock: hashesPerBlock,
		digestSize:     digestSize,
	}, nil
}

// 计算单个数据块的哈希值
func (v *VerityHash) hashBlock(data []byte) ([]byte, error) {

	// Initialize hash function
	var hasher hash.Hash
	switch v.config.HashAlgorithm {
	case HashAlgoSHA256:
		hasher = sha256.New()
	case HashAlgoSHA512:
		hasher = sha512.New()
	default:
		return nil, fmt.Errorf("unsupported hash algorithm")
	}

	// 写入盐值
	if len(v.config.Salt) > 0 {
		hasher.Write(v.config.Salt)
	}

	// 写入数据
	hasher.Write(data)

	return hasher.Sum(nil), nil
}

// 创建哈希树
func (v *VerityHash) GenerateHashTree(dataFile, hashFile string) ([]byte, error) {
	// 打开数据文件
	df, err := os.Open(dataFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open data file: %v", err)
	}
	defer df.Close()

	// 创建哈希文件
	hf, err := os.Create(hashFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create hash file: %v", err)
	}
	defer hf.Close()

	// 获取数据文件大小
	dataSize, err := df.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to get file size: %v", err)
	}
	df.Seek(0, io.SeekStart)

	// 计算数据块数量
	dataBlocks := uint64(math.Ceil(float64(dataSize) / float64(v.config.DataBlockSize)))

	// 计算哈希树层数
	levels := 0
	remainingBlocks := dataBlocks
	for remainingBlocks > 1 {
		remainingBlocks = (remainingBlocks + uint64(v.hashesPerBlock) - 1) / uint64(v.hashesPerBlock)
		levels++
	}

	// 创建临时缓冲区
	dataBuffer := make([]byte, v.config.DataBlockSize)

	// Create verity header (4096 bytes)
	header := make([]byte, 4096)

	// Fill header fields
	copy(header[0:8], []byte("verity\000\000"))
	binary.LittleEndian.PutUint32(header[8:], v.config.Version)
	binary.LittleEndian.PutUint32(header[12:], v.config.HashAlgorithm)

	// Generate UUID
	uuid := make([]byte, 16)
	if _, err := rand.Read(uuid); err != nil {
		return nil, fmt.Errorf("failed to generate UUID: %v", err)
	}
	copy(header[16:32], uuid)

	// Algorithm name
	algoName := "sha256"
	if v.config.HashAlgorithm == HashAlgoSHA512 {
		algoName = "sha512"
	}
	copy(header[32:], []byte(algoName+"\000"))

	// Block sizes and counts
	binary.LittleEndian.PutUint32(header[64:], v.config.DataBlockSize)
	binary.LittleEndian.PutUint32(header[68:], v.config.HashBlockSize)
	binary.LittleEndian.PutUint64(header[72:], dataBlocks)
	binary.LittleEndian.PutUint32(header[80:], uint32(v.digestSize))

	// Salt
	copy(header[0x58:], v.config.Salt)

	// Write header to hash file
	if _, err := hf.Write(header); err != nil {
		return nil, fmt.Errorf("failed to write header: %v", err)
	}

	// 处理第一层 - 数据块的哈希
	currentLevelHashes := make([][]byte, 0)
	hashBuf := make([]byte, 0, v.config.HashBlockSize)

	for i := uint64(0); i < dataBlocks; i++ {
		n, err := df.Read(dataBuffer)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read data block: %v", err)
		}

		// 计算数据块哈希
		hash, err := v.hashBlock(dataBuffer[:n])
		if err != nil {
			return nil, fmt.Errorf("failed to hash data block: %v", err)
		}

		currentLevelHashes = append(currentLevelHashes, hash)
		hashBuf = append(hashBuf, hash...)

		// 当hash buffer满了或者是最后一个块时，写入对齐的数据
		if len(hashBuf) == int(v.config.HashBlockSize) || i == dataBlocks-1 {
			// 创建对齐的buffer
			alignedBuf := make([]byte, v.config.HashBlockSize)
			copy(alignedBuf, hashBuf)

			// 写入对齐的数据
			if _, err := hf.Write(alignedBuf); err != nil {
				return nil, fmt.Errorf("failed to write hash block: %v", err)
			}
			hashBuf = hashBuf[:0]
		}
	}

	// 处理上层哈希
	for level := 1; level <= levels; level++ {
		nextLevelHashes := make([][]byte, 0)

		// 每 hashesPerBlock 个哈希组合成一个新的哈希
		for i := 0; i < len(currentLevelHashes); i += v.hashesPerBlock {
			end := i + v.hashesPerBlock
			if end > len(currentLevelHashes) {
				end = len(currentLevelHashes)
			}

			// 将多个哈希值连接起来
			combinedHash := make([]byte, 0)
			for _, hash := range currentLevelHashes[i:end] {
				combinedHash = append(combinedHash, hash...)
			}

			// 计算新的哈希值
			hash, err := v.hashBlock(combinedHash)
			if err != nil {
				return nil, fmt.Errorf("failed to hash block: %v", err)
			}

			nextLevelHashes = append(nextLevelHashes, hash)
			// log.Printf("level %d hash %d: %x", level, i, hash)
			// // 写入哈希文件
			// if _, err := hf.Write(hash); err != nil {
			// 	return nil, fmt.Errorf("failed to write hash: %v", err)
			// }
		}

		currentLevelHashes = nextLevelHashes
	}

	// 返回根哈希
	if len(currentLevelHashes) != 1 {
		return nil, fmt.Errorf("invalid root hash count")
	}

	return currentLevelHashes[0], nil
}
