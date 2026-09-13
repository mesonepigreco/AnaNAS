package publish

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
	"nas-sync/internal/journal"
)

// confirmMissingLeaf never removes content. It binds an observed absence to the
// still-reachable parent and records it durably before journal completion. A
// missing parent, replacement root, symlink or access error is not an absence.
func (p *Publisher) confirmMissingLeaf(ctx context.Context, e journal.Entry, parent *os.File, name string) error {
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.checkParent(e.Path, parent); err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
			return errors.Join(ErrExternal, err)
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	parentID, err := identity(parent)
	if err != nil {
		return err
	}
	if err := p.saveIdentityReceipt(e.Next.ID, ".absent", parentID); err != nil {
		return err
	}
	if p.afterAbsenceReceipt != nil {
		if err := p.afterAbsenceReceipt(); err != nil {
			return err
		}
	}
	return check()
}
