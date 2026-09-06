// Package hash provides content digests and fixed-size block hashing for the
// diff engine (PLAN.md §4, §5.4). Digests are BLAKE3: fast enough to make
// re-hashing cheap, and safe for content addressing.
package hash

import (
	"crypto/rand"
	"encoding/hex"
	"io"

	"github.com/zeebo/blake3"
)

// DigestSize is the length of a digest in bytes.
const DigestSize = 32

// DefaultBlockSize is the content-block granularity used by the diff engine.
const DefaultBlockSize = 64 * 1024

// Digest is a content-addressable identifier for a block or file.
type Digest [DigestSize]byte

// Hex returns the lowercase hex representation of the digest.
func (d Digest) Hex() string { return hex.EncodeToString(d[:]) }

// String implements fmt.Stringer.
func (d Digest) String() string { return d.Hex() }

// Sum hashes the entire stream r and returns its digest.
func Sum(r io.Reader) (Digest, error) {
	h := blake3.New()
	if _, err := io.Copy(h, r); err != nil {
		return Digest{}, err
	}
	return digestOf(h), nil
}

// SumBytes hashes a byte slice and returns its digest.
func SumBytes(b []byte) Digest {
	h := blake3.New()
	_, _ = h.Write(b) // hash writes never fail
	return digestOf(h)
}

func digestOf(h *blake3.Hasher) Digest {
	var d Digest
	copy(d[:], h.Sum(nil))
	return d
}

// RandomDigest returns a digest of random bytes (used as a lock/transaction id,
// never as a content digest).
func RandomDigest() (Digest, error) {
	var d Digest
	if _, err := rand.Read(d[:]); err != nil {
		return Digest{}, err
	}
	return d, nil
}

// Block is one fixed-size slice of a file together with its digest.
type Block struct {
	Index  int
	Digest Digest
	Size   int64
}

// Blocks reads r sequentially and splits it into blocks of blockSize bytes
// (the last block may be shorter). The returned slice has one Block per chunk,
// in file order. Hashing only ever reads each byte once.
func Blocks(r io.Reader, blockSize int64) ([]Block, error) {
	buf := make([]byte, blockSize)
	var out []Block
	for i := 0; ; i++ {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			out = append(out, Block{
				Index:  i,
				Digest: SumBytes(buf[:n]),
				Size:   int64(n),
			})
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// BlockOffsets returns the ordered list of digests only, mirroring Blocks but
// with an offset. Kept separate so callers that only need digests avoid the
// allocation overhead of the full Block slice when convenient.
func BlockDigests(r io.Reader, blockSize int64) ([]Digest, error) {
	blocks, err := Blocks(r, blockSize)
	if err != nil {
		return nil, err
	}
	ds := make([]Digest, len(blocks))
	for i, b := range blocks {
		ds[i] = b.Digest
	}
	return ds, nil
}
