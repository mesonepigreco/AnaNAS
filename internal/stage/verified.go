package stage

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/zeebo/blake3"
	"nas-sync/internal/hash"
)

// ReceiveVerified gives a producer only a bounded sequential staging writer.
// Its successful return, exact byte count and expected digest are all required
// before flushing/linking a ready object. A transport's late trailer or gate
// error therefore cannot leave a candidate marked ready. A late fsync error may
// still leave a ready name; callers must revalidate it through recovery.
func (s *Store) ReceiveVerified(ctx context.Context, id string, size int64, digest hash.Digest, produce func(io.Writer) error) error {
	_, err := s.capture(ctx, id, size, &digest, produce)
	return err
}

// Capture saves an exact-size local snapshot and returns its computed digest.
// The producer must validate source identity/generation before returning nil.
// Unlike ReceiveVerified this does not authenticate against an external digest.
func (s *Store) Capture(ctx context.Context, id string, size int64, produce func(io.Writer) error) (hash.Digest, error) {
	return s.capture(ctx, id, size, nil, produce)
}

func (s *Store) capture(ctx context.Context, id string, size int64, expected *hash.Digest, produce func(io.Writer) error) (got hash.Digest, err error) {
	if size < 0 || size > 8<<30 || produce == nil {
		return got, fmt.Errorf("bounded size and content producer required")
	}
	err = s.receive(ctx, id, func(f *os.File) error {
		w := &verifiedWriter{ctx: ctx, out: f, hash: blake3.New(), left: size}
		if err := produce(w); err != nil {
			return err
		}
		if w.failed != nil {
			return w.failed
		}
		if w.left != 0 {
			return fmt.Errorf("staged content is shorter than approved size")
		}
		w.hash.Sum(got[:0])
		if expected != nil && got != *expected {
			return fmt.Errorf("staged content digest differs")
		}
		return nil
	})
	if err != nil {
		return hash.Digest{}, err
	}
	return got, nil
}

type verifiedWriter struct {
	ctx    context.Context
	out    io.Writer
	hash   *blake3.Hasher
	left   int64
	failed error
}

func (w *verifiedWriter) Write(p []byte) (int, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if int64(len(p)) > w.left {
		w.failed = fmt.Errorf("staged content exceeds approved size")
		return 0, w.failed
	}
	total := 0
	for len(p) > 0 {
		if err := w.ctx.Err(); err != nil {
			w.failed = err
			return total, err
		}
		chunk := p[:min(len(p), 64*1024)]
		n, err := w.out.Write(chunk)
		if n > 0 {
			w.hash.Write(chunk[:n])
			w.left -= int64(n)
			total += n
			p = p[n:]
		}
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.failed = err
			return total, err
		}
	}
	return total, nil
}
