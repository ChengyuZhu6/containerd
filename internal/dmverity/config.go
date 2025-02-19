package dmverity

// Hash algorithms
const (
	HashAlgoSHA256 = 1
	HashAlgoSHA512 = 2

	// Default values
	DefaultBlockSize = 4096
	DefaultHashSize  = 32 // SHA256
	DefaultSaltSize  = 32
)

// VerityConfig contains the configuration for dm-verity
type VerityConfig struct {
	Version       uint32
	HashAlgorithm uint32
	DataBlockSize uint32
	HashBlockSize uint32
	DataBlocks    uint64
	Salt          []byte
	RootDigest    []byte
	HashOffset    int64
}
