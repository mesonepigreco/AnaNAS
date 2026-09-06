// Package hash provides content digests and fixed-size block hashing for the
// diff engine (PLAN.md §4, §5.4). Digests are BLAKE3: fast enough to make
// re-hashing cheap, and safe for content addressing.
package hash

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	return Digest(blake3.Sum256(b))
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

// MaxBlockSize bounds the working buffer even for untrusted manifests.
const MaxBlockSize = 4 * 1024 * 1024

// Stream hashes with one reusable buffer and delivers one digest at a time.
// Cancellation is checked between reads and callbacks; it cannot interrupt a
// blocked Reader. Network callers must additionally set transport deadlines.
// A callback may pace work, persist a digest, or stop by returning an error.
func Stream(ctx context.Context, r io.Reader, blockSize int64, emit func(Block) error) error {
	if blockSize <= 0 || blockSize > MaxBlockSize {
		return fmt.Errorf("block size must be in [1, %d]", MaxBlockSize)
	}
	if emit == nil {
		return fmt.Errorf("hash callback is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	buf := make([]byte, int(blockSize))
	reader := &contextReader{ctx: ctx, r: r}
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := io.ReadFull(reader, buf)
		if cancel := ctx.Err(); cancel != nil {
			return cancel
		}
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		if n > 0 {
			if e := emit(Block{Index: i, Digest: SumBytes(buf[:n]), Size: int64(n)}); e != nil {
				return e
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return ctx.Err()
		}
	}
}

type contextReader struct {
	ctx        context.Context
	r          io.Reader
	emptyReads int
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if n == 0 && err == nil {
		r.emptyReads++
		if r.emptyReads >= 100 {
			return 0, io.ErrNoProgress
		}
	} else {
		r.emptyReads = 0
	}
	return n, err
}

// Blocks is a convenience collector with O(number of blocks) memory. Use Stream
// for large files or untrusted input sizes.
func Blocks(r io.Reader, blockSize int64) ([]Block, error) {
	var out []Block
	err := Stream(context.Background(), r, blockSize, func(b Block) error {
		out = append(out, b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BlockDigests collects digests directly, without an intermediate Block slice.
func BlockDigests(r io.Reader, blockSize int64) ([]Digest, error) {
	var out []Digest
	err := Stream(context.Background(), r, blockSize, func(b Block) error {
		out = append(out, b.Digest)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
