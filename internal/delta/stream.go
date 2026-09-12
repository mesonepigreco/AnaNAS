package delta

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/zeebo/blake3"
	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"
)

const streamMagic = "NSDL0001"
const streamHeader = 60
const HeaderBytes = streamHeader
const MaxOperations = 3*manifest.MaxBlocks + 1

var ErrVerificationBudget = errors.New("rolling checksum verification budget exceeded; no automatic full-file fallback")

// Header permits a bounded transport to reject mismatched target/base metadata
// before creating staging output. Reading it does not verify the stream payload.
type Header struct {
	BlockSize            int
	BaseSize, TargetSize int64
	BaseDigest           hash.Digest
}

func InspectHeader(p []byte) (Header, error) {
	var h Header
	if len(p) != streamHeader || string(p[:8]) != streamMagic {
		return h, fmt.Errorf("invalid delta header")
	}
	h.BlockSize = int(binary.BigEndian.Uint32(p[8:12]))
	a, b := binary.BigEndian.Uint64(p[12:20]), binary.BigEndian.Uint64(p[20:28])
	if a > 1<<63-1 || b > 1<<63-1 {
		return h, fmt.Errorf("delta size overflows int64")
	}
	h.BaseSize, h.TargetSize = int64(a), int64(b)
	copy(h.BaseDigest[:], p[28:60])
	if err := validSize(h.BlockSize, h.BaseSize); err != nil {
		return h, err
	}
	if err := validSize(h.BlockSize, h.TargetSize); err != nil {
		return h, err
	}
	return h, nil
}

type Stats struct {
	LiteralBytes int64       `json:"literalBytes"`
	ReusedBytes  int64       `json:"reusedBytes"`
	Operations   int         `json:"operations"`
	StrongChecks int         `json:"strongChecks"`
	Digest       hash.Digest `json:"digest"`
}

type key struct {
	weak   uint32
	strong hash.Digest
}

func writeAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}

// Encode consumes a stable target in one pass. It searches a rolling window for
// full base blocks at arbitrary offsets, and emits bounded literal/copy frames.
// Source reads and transport writes must be paced/deadlined by the caller. A
// changed source or excessive weak-checksum collisions fail, never silently
// switch to a full-file transfer. The caller must verify source generation and
// obtain commit coordination after encoding; a valid stream is not a commit.
func Encode(ctx context.Context, src io.Reader, size int64, base *Signature, dst io.Writer) (Stats, error) {
	var stats Stats
	if err := base.Validate(); err != nil {
		return stats, err
	}
	if err := validSize(base.BlockSize, size); err != nil {
		return stats, err
	}
	if src == nil || dst == nil {
		return stats, fmt.Errorf("source and destination required")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	var header [streamHeader]byte
	copy(header[:], streamMagic)
	binary.BigEndian.PutUint32(header[8:12], uint32(base.BlockSize))
	binary.BigEndian.PutUint64(header[12:20], uint64(base.Size))
	binary.BigEndian.PutUint64(header[20:28], uint64(size))
	copy(header[28:60], base.Digest[:])
	if err := writeAll(dst, header[:]); err != nil {
		return stats, err
	}
	full := make(map[key]int64, len(base.Blocks))
	weak := make(map[uint32]struct{}, len(base.Blocks))
	for i, b := range base.Blocks {
		off := int64(i) * int64(base.BlockSize)
		if off+int64(base.BlockSize) > base.Size {
			break
		}
		k := key{b.Weak, b.Strong}
		if _, exists := full[k]; !exists {
			full[k] = off
		}
		weak[b.Weak] = struct{}{}
	}
	reader := &checkedReader{ctx: ctx, src: src}
	r := bufio.NewReaderSize(io.LimitReader(reader, size), 64*1024)
	window := make([]byte, base.BlockSize)
	literal := make([]byte, 0, base.BlockSize)
	whole := blake3.New()
	strong := blake3.New()
	var frame [45]byte
	emit := func(copyOffset int64, first, second []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		stats.Operations++
		if stats.Operations > MaxOperations {
			return fmt.Errorf("too many delta operations")
		}
		strong.Reset()
		strong.Write(first)
		strong.Write(second)
		var digest hash.Digest
		strong.Sum(digest[:0])
		n := len(first) + len(second)
		if copyOffset < 0 {
			frame[0] = 1
			binary.BigEndian.PutUint32(frame[1:5], uint32(n))
			copy(frame[5:37], digest[:])
			if err := writeAll(dst, frame[:37]); err != nil {
				return err
			}
			if err := writeAll(dst, first); err != nil {
				return err
			}
			if len(second) > 0 {
				if err := writeAll(dst, second); err != nil {
					return err
				}
			}
			stats.LiteralBytes += int64(n)
		} else {
			frame[0] = 2
			binary.BigEndian.PutUint64(frame[1:9], uint64(copyOffset))
			binary.BigEndian.PutUint32(frame[9:13], uint32(n))
			copy(frame[13:45], digest[:])
			if err := writeAll(dst, frame[:]); err != nil {
				return err
			}
			stats.ReusedBytes += int64(n)
		}
		whole.Write(first)
		whole.Write(second)
		return nil
	}
	flush := func() error {
		if len(literal) == 0 {
			return nil
		}
		if err := emit(-1, literal, nil); err != nil {
			return err
		}
		literal = literal[:0]
		return nil
	}
	appendLiteral := func(b byte) error {
		literal = append(literal, b)
		if len(literal) == cap(literal) {
			return flush()
		}
		return nil
	}
	readWindow := func() (int, error) {
		n, err := io.ReadFull(r, window)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err = nil
		}
		return n, err
	}
	n, err := readWindow()
	if err != nil {
		return stats, err
	}
	pos := 0
	sum := checksum(window[:n])
	checksLimit := int((size+int64(base.BlockSize)-1)/int64(base.BlockSize))*4 + 1024
	steps := 0
	for n == base.BlockSize {
		steps++
		if steps%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
		}
		matched := false
		if _, possible := weak[sum.value()]; possible {
			stats.StrongChecks++
			if stats.StrongChecks > checksLimit {
				return stats, ErrVerificationBudget
			}
			strong.Reset()
			strong.Write(window[pos:])
			strong.Write(window[:pos])
			var digest hash.Digest
			strong.Sum(digest[:0])
			if off, ok := full[key{sum.value(), digest}]; ok {
				if err := flush(); err != nil {
					return stats, err
				}
				if err := emit(off, window[pos:], window[:pos]); err != nil {
					return stats, err
				}
				n, err = readWindow()
				if err != nil {
					return stats, err
				}
				pos = 0
				sum = checksum(window[:n])
				matched = true
			}
		}
		if matched {
			continue
		}
		old := window[pos]
		if err := appendLiteral(old); err != nil {
			return stats, err
		}
		b, readErr := r.ReadByte()
		if readErr == io.EOF {
			pos = (pos + 1) % len(window)
			n--
			break
		}
		if readErr != nil {
			return stats, readErr
		}
		window[pos] = b
		pos = (pos + 1) % len(window)
		sum.shift(old, b, len(window))
	}
	// A partial base block can be reused as the target suffix. Other unmatched
	// short regions remain bounded literals; they never trigger whole-file copy.
	tailSize := int(base.Size % int64(base.BlockSize))
	for n > 0 {
		if n == tailSize && tailSize > 0 {
			end := min(pos+n, len(window))
			first, second := window[pos:end], window[:n-(end-pos)]
			strong.Reset()
			strong.Write(first)
			strong.Write(second)
			var digest hash.Digest
			strong.Sum(digest[:0])
			if digest == base.Blocks[len(base.Blocks)-1].Strong {
				if err := flush(); err != nil {
					return stats, err
				}
				if err := emit(base.Size-int64(tailSize), first, second); err != nil {
					return stats, err
				}
				n = 0
				break
			}
		}
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
		}
		if err := appendLiteral(window[pos]); err != nil {
			return stats, err
		}
		pos = (pos + 1) % len(window)
		n--
	}
	if err := flush(); err != nil {
		return stats, err
	}
	if stats.LiteralBytes+stats.ReusedBytes != size {
		return stats, io.ErrUnexpectedEOF
	}
	if err := requireEOF(reader); err != nil {
		return stats, err
	}
	whole.Sum(stats.Digest[:0])
	frame[0] = 0
	copy(frame[1:33], stats.Digest[:])
	if err := writeAll(dst, frame[:33]); err != nil {
		return stats, err
	}
	return stats, nil
}

// Apply reconstructs into a caller-owned staging writer using a receiver-local
// immutable base. Each frame is verified before writing; the whole target digest
// and EOF are verified before success. On any error the output is incomplete and
// MUST NOT be published. Caller owns fsync, CAS, journaling and recovery.
func Apply(ctx context.Context, wire io.Reader, base io.ReaderAt, baseSize int64, baseDigest hash.Digest, dst io.Writer) (Stats, error) {
	var stats Stats
	if wire == nil || dst == nil {
		return stats, fmt.Errorf("wire and staging destination required")
	}
	r := &checkedReader{ctx: ctx, src: wire}
	var header [streamHeader]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return stats, err
	}
	if string(header[:8]) != streamMagic {
		return stats, fmt.Errorf("unsupported delta format")
	}
	blockSize := int(binary.BigEndian.Uint32(header[8:12]))
	wireBase := binary.BigEndian.Uint64(header[12:20])
	target := binary.BigEndian.Uint64(header[20:28])
	if wireBase > 1<<63-1 || target > 1<<63-1 {
		return stats, fmt.Errorf("delta size overflows int64")
	}
	if err := validSize(blockSize, int64(wireBase)); err != nil {
		return stats, err
	}
	if err := validSize(blockSize, int64(target)); err != nil {
		return stats, err
	}
	var expectedBase hash.Digest
	copy(expectedBase[:], header[28:60])
	if baseSize != int64(wireBase) || baseDigest != expectedBase {
		return stats, fmt.Errorf("delta base changed")
	}
	buf := make([]byte, blockSize)
	whole := blake3.New()
	var frame [44]byte
	for {
		if _, err := io.ReadFull(r, frame[:1]); err != nil {
			return stats, err
		}
		tag := frame[0]
		if tag == 0 {
			if _, err := io.ReadFull(r, frame[:32]); err != nil {
				return stats, err
			}
			whole.Sum(stats.Digest[:0])
			var expected hash.Digest
			copy(expected[:], frame[:32])
			if stats.Digest != expected || stats.LiteralBytes+stats.ReusedBytes != int64(target) {
				return stats, fmt.Errorf("delta target verification failed")
			}
			if err := requireEOF(r); err != nil {
				return stats, err
			}
			return stats, nil
		}
		stats.Operations++
		if stats.Operations > MaxOperations {
			return stats, fmt.Errorf("too many delta operations")
		}
		var n uint32
		var off uint64
		var expected hash.Digest
		switch tag {
		case 1:
			if _, err := io.ReadFull(r, frame[:36]); err != nil {
				return stats, err
			}
			n = binary.BigEndian.Uint32(frame[:4])
			copy(expected[:], frame[4:36])
		case 2:
			if _, err := io.ReadFull(r, frame[:44]); err != nil {
				return stats, err
			}
			off = binary.BigEndian.Uint64(frame[:8])
			n = binary.BigEndian.Uint32(frame[8:12])
			copy(expected[:], frame[12:44])
		default:
			return stats, fmt.Errorf("unknown delta opcode")
		}
		remaining := int64(target) - stats.LiteralBytes - stats.ReusedBytes
		if n == 0 || n > uint32(blockSize) || int64(n) > remaining {
			return stats, fmt.Errorf("invalid delta frame length")
		}
		if tag == 1 {
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				return stats, err
			}
		} else {
			if base == nil || off > wireBase || uint64(n) > wireBase-off || off%uint64(blockSize) != 0 || (n != uint32(blockSize) && off+uint64(n) != wireBase) {
				return stats, fmt.Errorf("invalid base range")
			}
			if _, err := base.ReadAt(buf[:n], int64(off)); err != nil {
				return stats, err
			}
		}
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if hash.SumBytes(buf[:n]) != expected {
			return stats, fmt.Errorf("delta block verification failed")
		}
		if err := writeAll(dst, buf[:n]); err != nil {
			return stats, err
		}
		whole.Write(buf[:n])
		if tag == 1 {
			stats.LiteralBytes += int64(n)
		} else {
			stats.ReusedBytes += int64(n)
		}
	}
}
