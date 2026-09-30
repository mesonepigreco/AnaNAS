package replica

import (
	"context"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/diff"
	"nas-sync/internal/index"
)

// Verify newly published files before moving to the next remote batch. Inotify
// marks downloads dirty too; leaving those behind unrelated directory uploads
// would report zero local progress throughout a large initial download. Hashing
// is paced and exact-generation acknowledgement cannot erase concurrent edits.
func (w *Worker) verifyDownloaded(ctx context.Context, batch *changefeed.Batch) {
	for _, entry := range batch.Entries {
		if ctx.Err() != nil {
			return
		}
		if entry.Next.Directory || entry.Next.Tombstone {
			continue
		}
		record, ok, err := w.db.Get(entry.Path)
		if err != nil || !ok || !record.Dirty || record.Missing || record.Excluded {
			continue
		}
		w.verifyLocalRecord(ctx, record, entry.Next.ID)
	}
}

// VerifyLocalProgress reconciles old download observations independently of the
// network queue. It only acknowledges bytes matching an existing durable base;
// it never uploads, publishes files, or changes directory priority. The base and
// observed generation are checked atomically when clearing dirty work.
func (w *Worker) VerifyLocalProgress(ctx context.Context) error {
	if err := w.check(ctx); err != nil {
		return err
	}
	if w.opts.Task != nil {
		done := w.opts.Task()
		defer done()
	}
	after := ""
	for {
		records, err := w.db.DirtyPage(after, 128)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		for _, record := range records {
			if err := w.check(ctx); err != nil {
				return err
			}
			w.verifyLocalRecord(ctx, record, "")
			after = record.Path
		}
	}
}

func (w *Worker) verifyLocalRecord(ctx context.Context, record index.Record, expected string) {
	if !record.Dirty || record.Missing || record.Excluded {
		return
	}
	base, err := w.db.Base(record.Path)
	if err != nil || base == nil || base.Content.Directory || base.Content.Tombstone || (expected != "" && base.Content.ID != expected) {
		return
	}
	if err := w.check(ctx); err != nil {
		return
	}
	local, err := w.root.Hash(ctx, record.Path, record.Fingerprint, 65536, w.opts.ReadBytesPerSecond)
	if err != nil || diff.Plan(nil, &base.Content, &local.Content).Action != diff.Noop {
		return
	}
	if err := w.root.Verify(record.Path, record.Fingerprint); err != nil {
		return
	}
	w.db.Acknowledge(record.Path, base.Content.ID, record.Generation, base)
}
