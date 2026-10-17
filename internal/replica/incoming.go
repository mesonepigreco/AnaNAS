package replica

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/publish"
)

// incomingPublisher retains failed visible publications as durable decisions.
// Committing the logical remote head keeps the stream usable; the issue blocks
// local uploads for that path/subtree and pins the retained remote candidate.
type incomingPublisher struct {
	db    *index.DB
	c     *journal.Coordinator
	inner journal.Publisher
}

func incomingPathError(err error) bool {
	return errors.Is(err, publish.ErrExternal) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ELOOP)
}
func (p incomingPublisher) Publish(ctx context.Context, r journal.Record) error {
	for _, e := range r.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		blocked, err := p.db.IncomingBlocked(e.Path)
		if err != nil {
			return err
		}
		if blocked {
			if err := p.db.RecordIncoming(e, "A related local change needs a decision. The NAS version is retained; other paths continue syncing."); err != nil {
				return err
			}
			continue
		}
		one := r
		one.Entries = []journal.Entry{e}
		err = p.inner.Publish(ctx, one)
		if err == nil {
			continue
		}
		if !incomingPathError(err) {
			return err
		}
		if err := p.db.RecordIncoming(e, err.Error()); err != nil {
			return err
		}
	}
	return nil
}

type incomingResolver interface {
	PreserveForResolution(context.Context, string, string) error
	ApplyResolution(context.Context, journal.Entry, string) error
}

// Called only by the replica owner, never the HTTP/GTK thread. A durable request
// is replayed after restart; backup and publication steps are idempotent.
func (w *Worker) resolveIncoming(ctx context.Context) (bool, error) {
	issue, err := w.db.NextIncomingRequest()
	if err != nil || issue == nil {
		return false, err
	}
	base, err := w.db.Base(issue.Path)
	if err != nil {
		return false, err
	}
	if base == nil || base.Content.ID != issue.Revision {
		return false, nil
	} // finish inbox adoption first
	p, ok := w.puller.publisher.(incomingPublisher)
	if !ok {
		return false, fmt.Errorf("incoming recovery publisher unavailable")
	}
	var failure error
	if issue.Choice == "retry" {
		failure = p.inner.Publish(ctx, journal.Record{Proposal: journal.Proposal{Entries: []journal.Entry{{Path: issue.Path, Expected: issue.Expected, Next: issue.Remote}}}, Sequence: 1, Epoch: w.c.Epoch()})
	} else if resolver, ok := p.inner.(incomingResolver); ok {
		failure = resolver.PreserveForResolution(ctx, issue.Path, issue.Request)
		// Lexical order creates parents before children. Tombstones are already
		// absent after the root backup and need no recursive deletion.
		for after := ""; failure == nil; {
			entries, err := w.db.BaseEntries(issue.Path, after)
			if err != nil {
				failure = err
				break
			}
			if len(entries) == 0 {
				break
			}
			for _, e := range entries {
				if err := w.check(ctx); err != nil {
					failure = err
					break
				}
				v, _, err := w.c.VersionAt(e.Path, e.Next.ID)
				if err != nil {
					failure = err
					break
				}
				e.Next = *v
				if !e.Next.Tombstone {
					failure = resolver.ApplyResolution(ctx, e, issue.Request)
					if failure != nil {
						break
					}
				}
				after = e.Path
			}
		}
	} else {
		failure = fmt.Errorf("native recovery unavailable")
	}
	if errors.Is(failure, context.Canceled) || errors.Is(failure, ErrSuspended) {
		return false, failure
	}
	message := ""
	if failure != nil {
		message = "Recovery could not finish: " + failure.Error() + ". Saved local recovery copies are retained."
	}
	if err := w.db.FinishIncomingRequest(*issue, message, issue.Choice == "nas" && failure == nil); err != nil {
		return false, err
	}
	return true, nil
}
