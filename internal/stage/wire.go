package stage

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/zeebo/blake3"
	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
)

type WireInfo struct {
	Size   int64
	Digest hash.Digest
}

// WriteWire creates a separate, bounded encoded upload spool. Its producer must
// verify the delta's target metadata before returning success. Content ready
// objects and encoded spools have distinct names and cannot replace each other.
// The caller must retain operation provenance, enforce aggregate quota, and
// revalidate this object against that provenance after restart.
func (s *Store) WriteWire(ctx context.Context, id string, maxBytes int64, produce func(io.Writer) error) (WireInfo, error) {
	var info WireInfo
	if maxBytes < delta.HeaderBytes+33 || maxBytes > (8<<30)+int64(delta.MaxOperations)*45+delta.HeaderBytes+33 || produce == nil {
		return info, fmt.Errorf("bounded wire size and producer required")
	}
	err := s.receiveKind(ctx, id, ".wire", func(f *os.File) error {
		w := &verifiedWriter{ctx: ctx, out: f, hash: blake3.New(), left: maxBytes}
		if err := produce(w); err != nil {
			return err
		}
		if w.failed != nil {
			return w.failed
		}
		info.Size = maxBytes - w.left
		if info.Size < delta.HeaderBytes+33 {
			return fmt.Errorf("encoded wire is too short")
		}
		w.hash.Sum(info.Digest[:0])
		return nil
	})
	if err != nil {
		return WireInfo{}, err
	}
	return info, nil
}

func (s *Store) OpenWire(id string) (*os.File, error) { return s.openReadyKind(id, ".wire") }

// DiscardWirePartial requires proof that no live encoder owns this operation.
// It never removes verified wire/content objects.
func (s *Store) DiscardWirePartial(id string) error { return s.discardPartialKind(id, ".wire") }
