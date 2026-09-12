package content

import (
	"context"
	"errors"
	"fmt"
	"io"

	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
	"nas-sync/internal/stage"
)

type Snapshot struct {
	Manifest    *manifest.Manifest
	Version     journal.Version
	Fingerprint index.Fingerprint
}

// Snapshot captures one stable native file in a single paced source pass,
// computing block digests while staging computes the whole digest. Source path,
// root and fingerprint are checked before sealing the private candidate. This
// does not clear dirty work or grant upload permission: the scheduler must also
// check generations, policy and aggregate quota before preparing an outbox.
func (r *Root) Snapshot(ctx context.Context, path string, want index.Fingerprint, store *stage.Store, bytesPerSecond int64) (Snapshot, error) {
	id, err := hash.RandomDigest()
	if err != nil {
		return Snapshot{}, err
	}
	return r.SnapshotAs(ctx, path, want, store, id.Hex(), bytesPerSecond)
}

// SnapshotAs uses a candidate identity already reserved by the upload builder.
// The caller proves it was unused before durably claiming it. Creation remains
// exclusive; this method never adopts an existing ready object on retry.
func (r *Root) SnapshotAs(ctx context.Context, path string, want index.Fingerprint, store *stage.Store, id string, bytesPerSecond int64) (Snapshot, error) {
	var result Snapshot
	if store == nil || !manifest.ValidID(id) || bytesPerSecond < 1024 || bytesPerSecond > 1<<30 || want.Size < 0 || want.Size > 8<<30 {
		return result, fmt.Errorf("bounded snapshot and private store required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := r.checkRoot(); err != nil {
		return result, err
	}
	f, err := r.open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	if err := matches(f, want); err != nil {
		return result, err
	}
	m := &manifest.Manifest{BlockSize: 64 * 1024, Content: diff.Manifest{ID: id, Size: want.Size, Blocks: make([]diff.Block, 0, (want.Size+65535)/65536)}}
	digest, err := store.Capture(ctx, m.Content.ID, want.Size, func(out io.Writer) error {
		reader := &pacedReader{ctx: ctx, r: f, rate: bytesPerSecond}
		buffer := make([]byte, 64*1024)
		for left := want.Size; left > 0; {
			n := int(min(left, int64(len(buffer))))
			if _, err := io.ReadFull(reader, buffer[:n]); err != nil {
				return err
			}
			written, err := out.Write(buffer[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
			m.Content.Blocks = append(m.Content.Blocks, diff.Block{Digest: hash.SumBytes(buffer[:n]), Size: int64(n)})
			left -= int64(n)
		}
		var extra [1]byte
		if n, err := reader.Read(extra[:]); n != 0 || err != io.EOF {
			return errors.Join(ErrChanged, err)
		}
		if err := matches(f, want); err != nil {
			return err
		}
		if err := r.Verify(path, want); err != nil {
			return err
		}
		return m.Validate()
	})
	if err != nil {
		return result, err
	}
	return Snapshot{Manifest: m, Version: journal.Version{ID: m.Content.ID, Size: want.Size, Digest: digest}, Fingerprint: want}, nil
}
