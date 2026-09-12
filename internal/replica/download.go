package replica

import (
	"context"
	"fmt"
	"reflect"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
	"nas-sync/internal/stage"
)

// FinishDownload completes one received batch after matching local publication.
// It reads retained candidates only and leaves all observed dirty work intact;
// later local comparison can distinguish download-generated events from edits.
// The caller serializes replica operations and owns remote acknowledgements,
// aggregate quotas, conflict resolution and automatic scheduling.
func FinishDownload(ctx context.Context, db *index.DB, c *journal.Coordinator, store *stage.Store, namespace string, o PushOptions) error {
	if db == nil || c == nil || store == nil || !manifest.ValidID(namespace) || !o.Writes || o.Gate == nil {
		return fmt.Errorf("explicit download completion dependencies required")
	}
	if o.MaxFileBytes < 1 || o.MaxFileBytes > 8<<30 || o.MaxBatchBytes < o.MaxFileBytes || o.MaxBatchBytes > 8<<30 || o.ReadBytesPerSecond < 1024 || o.ReadBytesPerSecond > 1<<30 {
		return fmt.Errorf("bounded download completion limits required")
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
	state, err := db.RemoteState()
	if err != nil {
		return err
	}
	if state.Namespace == "" {
		return nil
	}
	if state.Namespace != namespace {
		return fmt.Errorf("download target differs from inbox")
	}
	batch, err := db.NextRemoteBatch()
	if err != nil || batch == nil {
		return err
	}
	if err := c.BindReplicaOrigin(namespace); err != nil {
		return err
	}
	var total int64
	for _, e := range batch.Entries {
		if matcher.Match(e.Path, e.Next.Directory || e.Next.Tombstone) {
			return fmt.Errorf("download completion path excluded")
		}
		if e.Next.Size > o.MaxFileBytes || e.Next.Size > o.MaxBatchBytes-total {
			return fmt.Errorf("download completion exceeds content limit")
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
			return fmt.Errorf("download completion paused")
		}
		current, err := db.NextRemoteBatch()
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, batch) {
			return index.ErrStale
		}
		for _, e := range batch.Entries {
			pathState, err := db.PathState(e.Path)
			if err != nil {
				return err
			}
			if pathState.Paused || pathState.Conflict != nil {
				return fmt.Errorf("download completion path paused or conflicted")
			}
			r, ok, err := db.Get(e.Path)
			if err != nil {
				return err
			}
			if ok && r.Excluded {
				return index.ErrStale
			}
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return err
	}
	if len(batch.Entries) != 0 {
		r, err := c.Operation(batch.ID)
		if err != nil {
			return err
		}
		if r == nil || !r.Committed || !reflect.DeepEqual(r.Proposal, batch.Proposal) {
			return fmt.Errorf("matching committed local publication required")
		}
		for _, e := range batch.Entries {
			v, pending, err := c.Head(e.Path)
			if err != nil {
				return err
			}
			if pending || v == nil || *v != e.Next {
				return journal.ErrConflict
			}
		}
	}
	manifests := make([]*manifest.Manifest, len(batch.Entries))
	pace := newPacer(ctx, o.ReadBytesPerSecond)
	for i, e := range batch.Entries {
		if err := o.Gate(ctx); err != nil {
			return err
		}
		manifests[i], err = retainedManifest(ctx, store, e.Next, pace)
		if err != nil {
			return err
		}
	}
	if err := check(ctx); err != nil {
		return err
	}
	return db.FinalizeDownload(namespace, batch.Sequence, batch.ID, manifests)
}
