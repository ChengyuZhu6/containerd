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
	"crypto"
	"errors"
	"fmt"
	"io"
	"os"
)

type VerityHash struct {
	config VerityConfig
	// 每个哈希块可以存储的哈希值数量
	hashesPerBlock int
	// 哈希值大小
	digestSize int
}

const (
	VerityMaxLevels = 63
)

// VerityParams contains parameters for verity hash calculation
type VerityParams struct {
	HashType      int
	HashName      string
	HashDevice    string
	DataDevice    string
	HashBlockSize uint64
	DataBlockSize uint64
	DataBlocks    int64
	HashPosition  int64
	Salt          []byte
}

func getBitsUp(u uint64) uint {
	i := uint(0)
	for (1 << i) < u {
		i++
	}
	return i
}

func getBitsDown(u uint64) uint {
	i := uint(0)
	for (u >> i) > 1 {
		i++
	}
	return i
}

func verifyZero(file *os.File, bytes int64) error {
	block := make([]byte, bytes)
	if _, err := io.ReadFull(file, block); err != nil {
		return fmt.Errorf("EIO while reading spare area: %v", err)
	}

	for i := range block {
		if block[i] != 0 {
			pos, _ := file.Seek(0, io.SEEK_CUR)
			return fmt.Errorf("spare area is not zeroed at position %d", pos-bytes)
		}
	}
	return nil
}

func verifyHashBlock(hashName string, version int, data []byte, salt []byte) ([]byte, error) {
	var hash crypto.Hash

	switch hashName {
	case "sha256":
		hash = crypto.SHA256
	case "sha512":
		hash = crypto.SHA512
	default:
		return nil, fmt.Errorf("unsupported hash algorithm: %s", hashName)
	}

	h := hash.New()

	if version == 1 {
		h.Write(salt)
	}

	h.Write(data)

	if version == 0 {
		h.Write(salt)
	}

	return h.Sum(nil), nil
}

func createOrVerify(params *VerityParams, verify bool, rootHash []byte) error {
	dataFile, err := os.Open(params.DataDevice)
	if err != nil {
		return fmt.Errorf("cannot open data device: %v", err)
	}
	defer dataFile.Close()

	var hashFile *os.File
	if verify {
		hashFile, err = os.Open(params.HashDevice)
	} else {
		hashFile, err = os.OpenFile(params.HashDevice, os.O_RDWR, 0)
	}
	if err != nil {
		return fmt.Errorf("cannot open hash device: %v", err)
	}
	defer hashFile.Close()

	// Calculate hash tree levels
	hashPerBlockBits := getBitsDown(params.HashBlockSize / uint64(len(rootHash)))
	if hashPerBlockBits == 0 {
		return errors.New("invalid hash block size")
	}

	levels := 0
	if params.DataBlocks > 0 {
		blocks := params.DataBlocks
		for hashPerBlockBits*uint(levels) < 64 && (blocks-1)>>(hashPerBlockBits*uint(levels)) > 0 {
			levels++
		}
	}

	if levels > VerityMaxLevels {
		return errors.New("too many tree levels for verity volume")
	}

	// Calculate hash level positions
	hashLevelBlock := make([]int64, levels)
	hashLevelSize := make([]int64, levels)
	hashPos := params.HashPosition

	for i := levels - 1; i >= 0; i-- {
		hashLevelBlock[i] = hashPos
		s := (params.DataBlocks + (1 << ((int64(i) + 1) * int64(hashPerBlockBits))) - 1) >> ((int64(i) + 1) * int64(hashPerBlockBits))
		hashLevelSize[i] = s
		hashPos += s
	}

	// Process each level
	calculatedHash := make([]byte, len(rootHash))

	for i := 0; i < levels; i++ {
		if i == 0 {
			err = processLevel(params, dataFile, hashFile, 0, hashLevelBlock[i], params.DataBlocks, verify, calculatedHash)
		} else {
			hashFile2, err := os.Open(params.HashDevice)
			if err != nil {
				return err
			}
			err = processLevel(params, hashFile2, hashFile, hashLevelBlock[i-1], hashLevelBlock[i], hashLevelSize[i-1], verify, calculatedHash)
			hashFile2.Close()
		}
		if err != nil {
			return err
		}
	}

	// Verify final root hash
	if verify {
		if !equal(calculatedHash, rootHash) {
			return errors.New("root hash verification failed")
		}
	} else {
		copy(rootHash, calculatedHash)
	}

	return nil
}

func processLevel(params *VerityParams, reader *os.File, writer *os.File,
	dataBlock int64, hashBlock int64, blocks int64, verify bool, hash []byte) error {

	dataBuffer := make([]byte, params.DataBlockSize)
	hashBuffer := make([]byte, params.HashBlockSize)

	hashPerBlock := uint64(1) << getBitsDown(params.HashBlockSize/uint64(len(hash)))

	for blocks > 0 {
		// Read data block
		if _, err := reader.ReadAt(dataBuffer, dataBlock*int64(params.DataBlockSize)); err != nil {
			return err
		}

		// Calculate hash
		calculatedHash, err := verifyHashBlock(params.HashName, params.HashType, dataBuffer, params.Salt)
		if err != nil {
			return err
		}

		if verify {
			// Read and verify stored hash
			if _, err := writer.ReadAt(hashBuffer, hashBlock*int64(params.HashBlockSize)); err != nil {
				return err
			}
			if !equal(calculatedHash, hashBuffer[:len(hash)]) {
				return errors.New("hash verification failed")
			}
		} else {
			// Write calculated hash
			copy(hashBuffer[:len(hash)], calculatedHash)
			if _, err := writer.WriteAt(hashBuffer, hashBlock*int64(params.HashBlockSize)); err != nil {
				return err
			}
		}

		blocks--
		dataBlock++
		hashBlock++
	}

	return nil
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// VerityCreate creates a new verity hash tree
func VerityCreate(params *VerityParams, rootHash []byte) error {
	return createOrVerify(params, false, rootHash)
}

// VerityVerify verifies an existing verity hash tree
func VerityVerify(params *VerityParams, rootHash []byte) error {
	return createOrVerify(params, true, rootHash)
}
