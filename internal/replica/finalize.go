package replica

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/zeebo/blake3"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/content"
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
	"nas-sync/internal/stage"
)

// FinishUpload completes one remotely committed outbox using retained snapshots,
// never live source bytes. It adopts logical replica heads without publishing
// visible files, then atomically updates index bases and clears the outbox.
// After verified adoption, budgeted replicas reclaim only the encoded wire spool
// before clearing the receipt/outbox. Restarts repeat without resending content.
// The caller must serialize replica work and provide private, disjoint state.
func FinishUpload(ctx context.Context, db *index.DB, c *journal.Coordinator, store *stage.Store, namespace string, o PushOptions) error {
	if db == nil || c == nil || store == nil || !manifest.ValidID(namespace) || !o.Writes || o.Gate == nil {
		return fmt.Errorf("explicit upload completion dependencies required")
	}
	if o.MaxFileBytes < 1 || o.MaxFileBytes > 8<<30 || o.MaxBatchBytes < o.MaxFileBytes || o.MaxBatchBytes > 8<<30 || o.ReadBytesPerSecond < 1024 || o.ReadBytesPerSecond > 1<<30 {
		return fmt.Errorf("bounded upload completion limits required")
	}
	matcher, err := changefeed.Patterns(o.Exclusions)
	if err != nil {
		return err
	}
	if err := c.CheckStore(store); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, contentTimeout(o.MaxBatchBytes, o.ReadBytesPerSecond))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	u, err := db.PendingUpload()
	if err != nil || u == nil {
		return err
	}
	if u.Namespace != namespace || u.Receipt == nil {
		return fmt.Errorf("matching durable upload receipt required")
	}
	var total int64
	for _, e := range u.Proposal.Entries {
		if matcher.Match(e.Path, e.Next.Directory || e.Next.Tombstone) {
			return fmt.Errorf("upload completion path excluded")
		}
		if e.Next.Size > o.MaxFileBytes || e.Next.Size > o.MaxBatchBytes-total {
			return fmt.Errorf("upload completion exceeds content limit")
		}
		total += e.Next.Size
	}
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := o.Gate(ctx); err != nil {
			return err
		}
		paused, err := db.Paused()
		if err != nil {
			return err
		}
		if paused {
			return fmt.Errorf("upload completion paused")
		}
		current, err := db.PendingUpload()
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, u) {
			return index.ErrStale
		}
		for i, e := range u.Proposal.Entries {
			state, err := db.PathState(e.Path)
			if err != nil {
				return err
			}
			if state.Paused || state.Conflict != nil {
				return fmt.Errorf("upload completion path paused or conflicted")
			}
			r, ok, err := db.Get(e.Path)
			if err != nil {
				return err
			}
			if !ok || r.Excluded || r.Generation < u.Generations[i] {
				return index.ErrStale
			}
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return err
	}
	var manifests []*manifest.Manifest
	verify := func(ctx context.Context) error {
		manifests = make([]*manifest.Manifest, len(u.Proposal.Entries))
		pace := newPacer(ctx, o.ReadBytesPerSecond)
		for i, e := range u.Proposal.Entries {
			if err := o.Gate(ctx); err != nil {
				return err
			}
			m, err := retainedManifest(ctx, store, e.Next, pace)
			if err != nil {
				return err
			}
			manifests[i] = m
		}
		return check(ctx)
	}
	if _, err := c.AdoptReplica(ctx, c.Epoch(), namespace, *committedUpload(u), verify); err != nil {
		return err
	}
	// Adoption may already be durable after a crash between the two databases.
	if manifests == nil {
		if err := verify(ctx); err != nil {
			return err
		}
	}
	if err := check(ctx); err != nil {
		return err
	}
	if err := c.ReleaseUploadWire(ctx, namespace, u.Proposal); err != nil {
		return err
	}
	return db.FinalizeUpload(namespace, u.Proposal.ID, manifests)
}

func retainedManifest(ctx context.Context, store *stage.Store, v journal.Version, pace *pacer) (*manifest.Manifest, error) {
	m := &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: v.ID, Size: v.Size, Directory: v.Directory, Tombstone: v.Tombstone}}
	if v.Directory || v.Tombstone {
		return m, m.Validate()
	}
	f, err := store.OpenReady(v.ID)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() != v.Size {
		return nil, fmt.Errorf("retained snapshot size differs")
	}
	whole := blake3.New()
	reader := io.TeeReader(io.LimitReader(&pacedReader{f, pace}, v.Size+1), whole)
	m.Content.Blocks = make([]diff.Block, 0, (v.Size+65535)/65536)
	err = hash.Stream(ctx, reader, 65536, func(b hash.Block) error {
		if len(m.Content.Blocks) == manifest.MaxBlocks {
			return fmt.Errorf("retained manifest exceeds block bound")
		}
		m.Content.Blocks = append(m.Content.Blocks, diff.Block{Digest: b.Digest, Size: int64(b.Size)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	var digest hash.Digest
	whole.Sum(digest[:0])
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if digest != v.Digest || content.Fingerprint(before) != content.Fingerprint(after) {
		return nil, fmt.Errorf("retained snapshot changed or digest differs")
	}
	if err := errors.Join(ctx.Err(), m.Validate()); err != nil {
		return nil, err
	}
	return m, nil
}
