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

// observedDirectory verifies one native directory identity without listing it.
// Child changes may alter size/mtime/ctime; they do not replace the directory.
func (p *Publisher) observedDirectory(ctx context.Context, path string, want index.Fingerprint) (directoryIdentity, error) {
	if err := ctx.Err(); err != nil {
		return directoryIdentity{}, err
	}
	if !os.FileMode(want.Mode).IsDir() || p.exclusions.Match(path, true) {
		return directoryIdentity{}, fmt.Errorf("eligible directory observation required")
	}
	parent, name, err := p.parent(path)
	if err != nil {
		return directoryIdentity{}, err
	}
	defer parent.Close()
	d, err := p.openDirectory(parent, name)
	if err != nil {
		return directoryIdentity{}, err
	}
	defer d.Close()
	got, err := identity(d)
	if err != nil {
		return got, err
	}
	if got.device != want.Device || got.inode != want.Inode {
		return got, ErrExternal
	}
	if err := p.checkParent(path, parent); err != nil {
		return got, err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return got, err
	}
	if named.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(named.Dev) != got.device || uint64(named.Ino) != got.inode {
		return got, ErrExternal
	}
	return got, nil
}

// MatchExternalDirectory compares the current native inode with the retained
// head receipt. It permits child metadata activity without feedback versions.
func (p *Publisher) MatchExternalDirectory(ctx context.Context, path string, want index.Fingerprint, version string) (bool, error) {
	got, err := p.observedDirectory(ctx, path, want)
	if err != nil {
		return false, err
	}
	prior, err := p.directoryReceipt(version)
	if err != nil {
		return false, err
	}
	return got == prior, nil
}

func (p *Publisher) captureExternalDirectory(ctx context.Context, e journal.Entry, want *index.Fingerprint, resume bool) (journal.Version, error) {
	v := e.Next
	if v.Size != 0 || !v.Directory || v.Tombstone {
		return v, fmt.Errorf("directory version required")
	}
	if resume {
		// As with a retained file snapshot, an earlier sealed receipt can be
		// committed even if a newer native change now awaits observation.
		if _, err := p.directoryReceipt(v.ID); err == nil {
			return v, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return v, err
		}
	}
	if want == nil {
		return v, fmt.Errorf("fresh directory observation required")
	}
	got, err := p.observedDirectory(ctx, e.Path, *want)
	if err != nil {
		return v, err
	}
	if err := ctx.Err(); err != nil {
		return v, err
	}
	return v, p.saveDirectoryReceipt(v.ID, got)
}
