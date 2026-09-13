package replica

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"nas-sync/internal/content"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

type localPathError struct {
	Path       string
	Generation uint64
	Err        error
}

func (e *localPathError) Error() string { return fmt.Sprintf("local file %q: %v", e.Path, e.Err) }
func (e *localPathError) Unwrap() error { return e.Err }

func isolatable(err error) bool {
	return errors.Is(err, content.ErrChanged) || errors.Is(err, content.ErrUnsupported) || errors.Is(err, index.ErrStale) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, journal.ErrConflict) || errors.Is(err, journal.ErrPending) || errors.Is(err, ErrAttention) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EXDEV)
}

// Defer only source-specific failures before an outbox exists. All temporary
// candidates must be released through the provenance-checked abort path first.
func (w *Worker) deferLocalFailure(ctx context.Context, err error, pass *workerPass) (bool, error) {
	var failure *localPathError
	if !errors.As(err, &failure) || failure.Generation == 0 || !isolatable(failure.Err) {
		return false, nil
	}
	if err := w.root.CheckAvailable(); err != nil {
		return false, fmt.Errorf("%w: local root unavailable: %v", ErrAttention, err)
	}
	if u, e := w.db.PendingUpload(); e != nil {
		return false, e
	} else if u != nil {
		return false, nil
	}
	if e := AbortUploadPreparation(ctx, w.db, w.c, w.store, w.opts.Namespace); e != nil {
		return false, fmt.Errorf("clean up failed local preparation: %w", e)
	}
	r, ok, e := w.db.Get(failure.Path)
	if e != nil {
		return false, e
	}
	if !ok {
		return false, nil
	}
	if errors.Is(err, content.ErrChanged) && r.Generation == failure.Generation {
		fp, missing, e := w.root.Metadata(r.Path)
		if e == nil {
			r.Missing = missing
			if !missing {
				r.Fingerprint = fp
			}
			if e := w.db.Put([]index.Record{r}, false); e != nil {
				return false, e
			}
			r, _, e = w.db.Get(r.Path)
			if e != nil {
				return false, e
			}
		}
	}
	retry := errors.Is(err, content.ErrChanged) || errors.Is(err, index.ErrStale) || errors.Is(err, journal.ErrPending) || r.Generation != failure.Generation
	reason := err.Error()
	if retry {
		reason = "Will retry: " + reason
	} else {
		reason = "Blocked: " + reason
	}
	if e := w.db.RecordSyncIssue(r.Path, r.Generation, reason, retry); e != nil {
		return false, e
	}
	if retry {
		pass.retry = fmt.Errorf("%w: %s", index.ErrStale, reason)
	} else {
		pass.attention = fmt.Errorf("%w: %s; other eligible files continue", ErrAttention, reason)
	}
	return true, nil
}
