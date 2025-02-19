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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateHashTree(t *testing.T) {
	// Create test data file
	dataFile, err := os.CreateTemp("", "verity-data-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dataFile.Name())

	// Write test data
	testData := make([]byte, DefaultBlockSize)
	copy(testData, []byte("test data"))
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
		Salt:          make([]byte, DefaultSaltSize),
	}

	// Generate hash tree
	hashTree, rootHash, err := GenerateHashTree(dataFile.Name(), config)
	if err != nil {
		t.Fatal(err)
	}

	// Verify results
	assert.NotNil(t, hashTree)
	assert.NotEmpty(t, hashTree)
	assert.NotNil(t, rootHash)
	assert.Len(t, rootHash, DefaultHashSize)
}

func TestBlockDeviceSize(t *testing.T) {
	// Create test file
	f, err := os.CreateTemp("", "blockdev-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	// Write some data
	testSize := int64(DefaultBlockSize * 2)
	if err := f.Truncate(testSize); err != nil {
		t.Fatal(err)
	}

	// Get size
	size, err := BlockDeviceSize(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, testSize, size)
}
