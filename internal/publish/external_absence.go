package publish

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

// captureExternalAbsence records a confirmed missing regular-file leaf without
// altering the visible tree. A missing parent, inaccessible root, symlink or
// changed mount is never interpreted as a deletion. The retained base remains
// available to replicas and conflict handling.
func (p *Publisher) captureExternalAbsence(ctx context.Context, e journal.Entry, want *index.Fingerprint, resume bool) (journal.Version, error) {
	v := e.Next
	if !v.Tombstone || v.Directory || v.Size != 0 || e.Expected == "" {
		return v, fmt.Errorf("external deletion requires an explicit regular-file base")
	}
	base, err := p.lookup(e.Expected)
	if err != nil {
		return v, err
	}
	if base == nil || base.Tombstone || base.Directory {
		return v, fmt.Errorf("directory deletion requires children-first reconciliation")
	}
	if resume {
		// A sealed absence is historical evidence, like a retained file snapshot.
		// A later native creation is observed separately after this commit.
		if _, err := p.identityReceipt(v.ID, ".absent"); err == nil {
			return v, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return v, err
		}
	}
	if want == nil || !os.FileMode(want.Mode).IsRegular() {
		return v, fmt.Errorf("fresh missing regular-file observation required")
	}
	parent, name, err := p.parent(e.Path)
	if err != nil {
		return v, err
	}
	defer parent.Close()
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return v, errors.Join(ErrExternal, err)
	}
	if err := p.checkParent(e.Path, parent); err != nil {
		return v, err
	}
	// Flush the observed unlink before publishing its durable journal record.
	if err := parent.Sync(); err != nil {
		return v, err
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return v, errors.Join(ErrExternal, err)
	}
	identity, err := identity(parent)
	if err != nil {
		return v, err
	}
	if err := ctx.Err(); err != nil {
		return v, err
	}
	return v, p.saveIdentityReceipt(v.ID, ".absent", identity)
}
