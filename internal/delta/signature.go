// Package delta implements a bounded rolling delta stream. A receiver reuses
// its own base file; unchanged content never needs a round trip to the sender.
// It does not select paths, publish files, authorize networking or resolve conflicts.
package delta

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/zeebo/blake3"
	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"
)

const signatureMagic = "NSSG0001"
const signatureHeader = 56
const MaxSignatureBytes = signatureHeader + 36*manifest.MaxBlocks

type Block struct {
	Weak   uint32
	Strong hash.Digest
}

// Signature describes fixed blocks of an immutable base. A short last block is
// matched at the target's end; full blocks can be found at any target offset.
type Signature struct {
	BlockSize int
	Size      int64
	Digest    hash.Digest
	Blocks    []Block
}

func validSize(blockSize int, size int64) error {
	if blockSize < 1024 || blockSize > hash.MaxBlockSize || blockSize&(blockSize-1) != 0 {
		return fmt.Errorf("invalid delta block size")
	}
	if size < 0 || size > int64(blockSize)*manifest.MaxBlocks {
		return fmt.Errorf("delta file exceeds block count limit")
	}
	return nil
}

func (s *Signature) Validate() error {
	if s == nil {
		return fmt.Errorf("base signature is required")
	}
	if err := validSize(s.BlockSize, s.Size); err != nil {
		return err
	}
	if len(s.Blocks) != int((s.Size+int64(s.BlockSize)-1)/int64(s.BlockSize)) {
		return fmt.Errorf("signature block count does not match size")
	}
	return nil
}

type rolling struct{ a, b uint32 }

func checksum(p []byte) rolling {
	var r rolling
	for _, x := range p {
		r.a += uint32(x)
		r.b += r.a
	}
	r.a &= 65535
	r.b &= 65535
	return r
}
func (r rolling) value() uint32 { return r.b<<16 | r.a }
func (r *rolling) shift(old, next byte, size int) {
	r.a = (r.a - uint32(old) + uint32(next)) & 65535
	r.b = (r.b - uint32(size)*uint32(old) + r.a) & 65535
}

// Build reads exactly size bytes, once, with one block buffer. Caller must pace
// the reader, verify the file generation before/after, and enforce I/O deadlines.
func Build(ctx context.Context, src io.Reader, size int64, blockSize int) (*Signature, error) {
	if err := validSize(blockSize, size); err != nil {
		return nil, err
	}
	if src == nil {
		return nil, fmt.Errorf("source is required")
	}
	s := &Signature{BlockSize: blockSize, Size: size, Blocks: make([]Block, 0, (size+int64(blockSize)-1)/int64(blockSize))}
	r := &checkedReader{ctx: ctx, src: src}
	buf := make([]byte, blockSize)
	whole := blake3.New()
	for remaining := size; remaining > 0; {
		n := min(int64(blockSize), remaining)
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			return nil, err
		}
		whole.Write(buf[:n])
		s.Blocks = append(s.Blocks, Block{checksum(buf[:n]).value(), hash.SumBytes(buf[:n])})
		remaining -= n
	}
	if err := requireEOF(r); err != nil {
		return nil, err
	}
	whole.Sum(s.Digest[:0])
	return s, nil
}

func (s *Signature) MarshalBinary() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	data := make([]byte, signatureHeader+36*len(s.Blocks))
	copy(data, signatureMagic)
	binary.BigEndian.PutUint32(data[8:12], uint32(s.BlockSize))
	binary.BigEndian.PutUint32(data[12:16], uint32(len(s.Blocks)))
	binary.BigEndian.PutUint64(data[16:24], uint64(s.Size))
	copy(data[24:56], s.Digest[:])
	for i, b := range s.Blocks {
		off := signatureHeader + i*36
		binary.BigEndian.PutUint32(data[off:off+4], b.Weak)
		copy(data[off+4:off+36], b.Strong[:])
	}
	return data, nil
}

func ParseSignature(data []byte) (*Signature, error) {
	if len(data) < signatureHeader || len(data) > MaxSignatureBytes || string(data[:8]) != signatureMagic {
		return nil, fmt.Errorf("invalid delta signature header")
	}
	blockSize := int(binary.BigEndian.Uint32(data[8:12]))
	count := binary.BigEndian.Uint32(data[12:16])
	size := binary.BigEndian.Uint64(data[16:24])
	if size > 1<<63-1 || count > manifest.MaxBlocks || len(data) != signatureHeader+36*int(count) {
		return nil, fmt.Errorf("invalid delta signature size")
	}
	if err := validSize(blockSize, int64(size)); err != nil {
		return nil, err
	}
	if uint64(count) != (size+uint64(blockSize)-1)/uint64(blockSize) {
		return nil, fmt.Errorf("invalid delta block count")
	}
	s := &Signature{BlockSize: blockSize, Size: int64(size), Blocks: make([]Block, int(count))}
	copy(s.Digest[:], data[24:56])
	for i := range s.Blocks {
		off := signatureHeader + 36*i
		s.Blocks[i].Weak = binary.BigEndian.Uint32(data[off : off+4])
		copy(s.Blocks[i].Strong[:], data[off+4:off+36])
	}
	return s, nil
}

type checkedReader struct {
	ctx   context.Context
	src   io.Reader
	empty int
}

func (r *checkedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.src.Read(p)
	if n == 0 && err == nil {
		r.empty++
		if r.empty >= 100 {
			return 0, io.ErrNoProgress
		}
	} else {
		r.empty = 0
	}
	if cancel := r.ctx.Err(); cancel != nil {
		return n, cancel
	}
	return n, err
}
func requireEOF(r io.Reader) error {
	var extra [1]byte
	n, err := io.ReadFull(r, extra[:])
	if n == 0 && err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected trailing data")
}
