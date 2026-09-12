// Package manifest defines the bounded, versioned encoding of content manifests.
// It performs no file or network I/O. Version identities are opaque random IDs;
// neither identity nor causality depends on a machine's wall clock.
package manifest

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
)

const (
	// MaxBlocks bounds one decoded manifest to roughly 5 MiB of block metadata.
	// With the default 64 KiB blocks this permits files up to 8 GiB.
	MaxBlocks      = 131072
	headerSize     = 32
	entrySize      = hash.DigestSize + 4
	MaxEncodedSize = headerSize + 64 + entrySize*MaxBlocks
)

// The header fixes both the format (1) and digest algorithm (1 = BLAKE3-256).
const magic = "NSMF\x00\x01\x00\x01"

type Manifest struct {
	BlockSize int
	Content   diff.Manifest
}

func ValidID(id string) bool {
	if len(id) != 64 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("manifest is required")
	}
	if !ValidID(m.Content.ID) {
		return fmt.Errorf("version ID must be 64 lowercase hexadecimal characters")
	}
	if m.BlockSize < 1024 || m.BlockSize > hash.MaxBlockSize || m.BlockSize&(m.BlockSize-1) != 0 {
		return fmt.Errorf("manifest block size must be a power of two in [1024,4194304]")
	}
	if err := m.Content.Validate(MaxBlocks); err != nil {
		return err
	}
	for i, b := range m.Content.Blocks {
		if b.Size > int64(m.BlockSize) || (i < len(m.Content.Blocks)-1 && b.Size != int64(m.BlockSize)) {
			return fmt.Errorf("block %d does not match fixed block layout", i)
		}
	}
	return nil
}

func Encode(m *Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, headerSize+64+entrySize*len(m.Content.Blocks))
	copy(b, magic)
	if m.Content.Tombstone {
		b[8] = 1
	} else if m.Content.Directory {
		b[8] = 2
	}
	binary.BigEndian.PutUint32(b[12:16], uint32(m.BlockSize))
	binary.BigEndian.PutUint64(b[16:24], uint64(m.Content.Size))
	binary.BigEndian.PutUint32(b[24:28], uint32(len(m.Content.Blocks)))
	copy(b[headerSize:], m.Content.ID)
	for i, block := range m.Content.Blocks {
		offset := headerSize + 64 + i*entrySize
		copy(b[offset:], block.Digest[:])
		binary.BigEndian.PutUint32(b[offset+hash.DigestSize:], uint32(block.Size))
	}
	return b, nil
}

// Decode checks lengths, format, algorithm and block count before allocation.
// Unknown formats fail closed; trailing bytes and nonzero reserved fields fail.
func Decode(b []byte) (*Manifest, error) {
	if len(b) < headerSize+64 || len(b) > MaxEncodedSize || string(b[:8]) != magic {
		return nil, fmt.Errorf("invalid or unsupported manifest header")
	}
	if b[8] > 2 || b[9] != 0 || b[10] != 0 || b[11] != 0 || binary.BigEndian.Uint32(b[28:32]) != 0 {
		return nil, fmt.Errorf("unsupported manifest flags")
	}
	count := binary.BigEndian.Uint32(b[24:28])
	if count > MaxBlocks || len(b) != headerSize+64+int(count)*entrySize {
		return nil, fmt.Errorf("invalid manifest block count or length")
	}
	size := binary.BigEndian.Uint64(b[16:24])
	if size > 1<<63-1 {
		return nil, fmt.Errorf("manifest size overflows int64")
	}
	m := &Manifest{BlockSize: int(binary.BigEndian.Uint32(b[12:16])), Content: diff.Manifest{
		ID: string(b[headerSize : headerSize+64]), Size: int64(size), Tombstone: b[8] == 1, Directory: b[8] == 2,
		Blocks: make([]diff.Block, int(count)),
	}}
	for i := range m.Content.Blocks {
		offset := headerSize + 64 + i*entrySize
		copy(m.Content.Blocks[i].Digest[:], b[offset:offset+hash.DigestSize])
		m.Content.Blocks[i].Size = int64(binary.BigEndian.Uint32(b[offset+hash.DigestSize:]))
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}
