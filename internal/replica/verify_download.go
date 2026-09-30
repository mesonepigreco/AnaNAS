package replica

import (
	"context"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/diff"
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
		base, err := w.db.Base(entry.Path)
		if err != nil || base == nil || base.Content.ID != entry.Next.ID {
			continue
		}
		if err := w.check(ctx); err != nil {
			return
		}
		local, err := w.root.Hash(ctx, entry.Path, record.Fingerprint, 65536, w.opts.ReadBytesPerSecond)
		if err != nil || diff.Plan(nil, &base.Content, &local.Content).Action != diff.Noop {
			continue
		}
		if err := w.root.Verify(entry.Path, record.Fingerprint); err != nil {
			continue
		}
		// This is only a local verification; failure leaves the ordinary dirty work
		// intact and must not undo an already durable download receipt.
		w.db.Acknowledge(entry.Path, base.Content.ID, record.Generation, base)
	}
}
