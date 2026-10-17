package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

// PreserveForResolution moves the local item into private recovery storage,
// without overwriting anything. The durable request ID makes restart replay
// distinguish the saved item from a newly published or edited visible version.
func (p *Publisher) PreserveForResolution(ctx context.Context, path, id string) error {
	if !manifest.ValidID(id) {
		return fmt.Errorf("invalid recovery ID")
	}
	if _, err := p.identityReceipt(id, ".resolution"); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent, name, err := p.parent(path)
	if errors.Is(err, unix.ENOENT) {
		// A missing ancestor is already absent; independently revalidate the
		// configured root before recording this no-op recovery step.
		root, _, rootErr := p.parent(strings.Split(path, "/")[0])
		if rootErr != nil {
			return rootErr
		}
		defer root.Close()
		identity, rootErr := identity(root)
		if rootErr != nil {
			return rootErr
		}
		return p.saveIdentityReceipt(id, ".resolution", identity)
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	parentID, err := identity(parent)
	if err != nil {
		return err
	}
	if err := unix.Mkdirat(int(p.state.Fd()), "recovered", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	recovery, err := p.openDirectory(p.state, "recovered")
	if err != nil {
		return err
	}
	defer recovery.Close()
	backup := name + "-" + id
	var st unix.Stat_t
	if err := unix.Fstatat(int(recovery.Fd()), backup, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		// The rename completed before the marker was flushed.
		if st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrExternal
		}
		if err := errors.Join(recovery.Sync(), parent.Sync()); err != nil {
			return err
		}
		return p.saveIdentityReceipt(id, ".resolution", parentID)
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return p.saveIdentityReceipt(id, ".resolution", parentID)
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	before, err := stat(f)
	if err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFDIR && before.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrExternal
	}
	m, err := mountID(f)
	if err != nil || m != p.mount {
		return fmt.Errorf("recovery item crosses mount")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.checkParent(path, parent); err != nil {
		return err
	}
	if err := unix.Renameat2(int(parent.Fd()), name, int(recovery.Fd()), backup, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := unix.Fstatat(int(recovery.Fd()), backup, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Dev != before.Dev || st.Ino != before.Ino {
		return ErrExternal
	}
	if err := errors.Join(f.Sync(), parent.Sync(), recovery.Sync(), p.state.Sync()); err != nil {
		return err
	}
	return p.saveIdentityReceipt(id, ".resolution", parentID)
}

// ApplyResolution publishes a retained remote version into the backed-up tree.
// Existing/new local edits still undergo the usual publication checks.
func (p *Publisher) ApplyResolution(ctx context.Context, e journal.Entry, request string) error {
	if err := journal.ValidateVersion(e.Next); err != nil {
		return err
	}
	if !manifest.ValidID(request) {
		return fmt.Errorf("invalid recovery ID")
	}
	if e.Next.Tombstone {
		return fmt.Errorf("recovery must not recursively delete")
	}
	e.Expected = ""
	if e.Next.Directory {
		parent, name, err := p.parent(e.Path)
		if err != nil {
			return err
		}
		defer parent.Close()
		d, err := p.openDirectory(parent, name)
		if err == nil {
			d.Close()
			return p.apply(ctx, e)
		}
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
		// Preserve a receipt bound to the backed-up inode before establishing
		// the new managed directory. Never replace it twice for this request.
		if _, err := p.directoryReceipt(e.Next.ID); err == nil {
			if err := unix.Renameat2(int(p.state.Fd()), e.Next.ID+".directory", int(p.state.Fd()), request+"-"+e.Next.ID+".directory-backup", unix.RENAME_NOREPLACE); err != nil {
				return err
			}
			if err := p.state.Sync(); err != nil {
				return err
			}
		} else if !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return p.apply(ctx, e)
}
