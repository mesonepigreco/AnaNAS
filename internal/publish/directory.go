package publish

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
	"nas-sync/internal/journal"
)

type directoryIdentity struct{ device, inode uint64 }

func identity(f *os.File) (directoryIdentity, error) {
	s, err := stat(f)
	return directoryIdentity{uint64(s.Dev), uint64(s.Ino)}, err
}

func (p *Publisher) openDirectory(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	m, err := mountID(f)
	if err != nil || m != p.mount {
		f.Close()
		return nil, fmt.Errorf("directory mount identity differs")
	}
	return f, nil
}

// A fixed-size durable receipt binds an app-created directory version to its
// native inode. It is independent of directory mtime, which child edits change.
func (p *Publisher) directoryReceipt(id string) (directoryIdentity, error) {
	return p.identityReceipt(id, ".directory")
}

func (p *Publisher) identityReceipt(id, suffix string) (directoryIdentity, error) {
	f, err := openRegular(p.state, id+suffix)
	if err != nil {
		return directoryIdentity{}, err
	}
	defer f.Close()
	s, err := stat(f)
	if err != nil {
		return directoryIdentity{}, err
	}
	if s.Size != 16 || s.Mode&0777 != 0400 || s.Uid != uint32(os.Geteuid()) || s.Nlink != 1 {
		return directoryIdentity{}, fmt.Errorf("invalid directory receipt")
	}
	m, err := mountID(f)
	if err != nil || m != p.mount {
		return directoryIdentity{}, fmt.Errorf("directory receipt mount differs")
	}
	var data [16]byte
	if _, err := io.ReadFull(f, data[:]); err != nil {
		return directoryIdentity{}, err
	}
	return directoryIdentity{binary.BigEndian.Uint64(data[:8]), binary.BigEndian.Uint64(data[8:])}, nil
}
func (p *Publisher) saveDirectoryReceipt(id string, want directoryIdentity) error {
	return p.saveIdentityReceipt(id, ".directory", want)
}

func (p *Publisher) saveIdentityReceipt(id, suffix string, want directoryIdentity) error {
	old, err := p.identityReceipt(id, suffix)
	if err == nil {
		if old != want {
			return ErrExternal
		}
		return nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return err
	}
	name := id + suffix
	temp := name + "-writing"
	if err := unix.Unlinkat(int(p.state.Fd()), temp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	fd, err := unix.Openat(int(p.state.Fd()), temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0400)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), temp)
	var data [16]byte
	binary.BigEndian.PutUint64(data[:8], want.device)
	binary.BigEndian.PutUint64(data[8:], want.inode)
	n, err := f.Write(data[:])
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := unix.Renameat2(int(p.state.Fd()), temp, int(p.state.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return p.state.Sync()
}

func (p *Publisher) directory(ctx context.Context, e journal.Entry, parent *os.File, name string, base *journal.Version) error {
	if !e.Next.Directory && !e.Next.Tombstone {
		return fmt.Errorf("directory-to-file replacement requires explicit deletion first")
	}
	if e.Next.Directory && base != nil && !base.Tombstone && !base.Directory {
		return fmt.Errorf("file-to-directory replacement requires explicit deletion first")
	}
	if p.exclusions.Match(e.Path, true) {
		return fmt.Errorf("directory is excluded")
	}
	visible, err := p.openDirectory(parent, name)
	absent := errors.Is(err, unix.ENOENT)
	if err != nil && !absent {
		return err
	}
	if visible != nil {
		defer visible.Close()
	}
	if e.Next.Tombstone {
		return p.removeDirectory(ctx, e, parent, name, visible, base, absent)
	}
	if !absent {
		want, err := identity(visible)
		if err != nil {
			return err
		}
		prior, receiptErr := p.directoryReceipt(e.Next.ID)
		if receiptErr == nil {
			if prior != want {
				return ErrExternal
			}
			return errors.Join(visible.Sync(), parent.Sync(), p.state.Sync())
		}
		if !errors.Is(receiptErr, unix.ENOENT) {
			return receiptErr
		}
		// A directory-only metadata version may retain the same managed inode.
		// An untracked same-name directory is left for explicit reconciliation.
		if base == nil || !base.Directory {
			return ErrExternal
		}
		prior, err = p.directoryReceipt(base.ID)
		if err != nil {
			return err
		}
		if prior != want {
			return ErrExternal
		}
		if err := p.saveDirectoryReceipt(e.Next.ID, want); err != nil {
			return err
		}
		return errors.Join(visible.Sync(), parent.Sync(), p.state.Sync())
	}
	if base != nil && !base.Tombstone {
		return ErrExternal
	}
	temp := e.Next.ID + ".mkdir"
	prepared, err := p.openDirectory(p.state, temp)
	if errors.Is(err, unix.ENOENT) {
		if _, receiptErr := p.directoryReceipt(e.Next.ID); receiptErr == nil {
			return ErrExternal
		} else if !errors.Is(receiptErr, unix.ENOENT) {
			return receiptErr
		}
		if err := unix.Mkdirat(int(p.state.Fd()), temp, 0700); err != nil {
			return err
		}
		prepared, err = p.openDirectory(p.state, temp)
	}
	if err != nil {
		return err
	}
	defer prepared.Close()
	s, err := stat(prepared)
	if err != nil {
		return err
	}
	if s.Mode&0777 != 0700 || s.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unsafe staged directory")
	}
	// Read only this private, newly staged directory. Never enumerate a visible
	// directory to decide deletion; rmdir below makes that decision atomically.
	entries, err := prepared.ReadDir(1)
	if err != io.EOF || len(entries) != 0 {
		return fmt.Errorf("staged directory is not empty")
	}
	want, err := identity(prepared)
	if err != nil {
		return err
	}
	if err := prepared.Sync(); err != nil {
		return err
	}
	if err := p.saveDirectoryReceipt(e.Next.ID, want); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.checkParent(e.Path, parent); err != nil {
		return err
	}
	if p.beforeRename != nil {
		if err := p.beforeRename(); err != nil {
			return err
		}
	}
	if err := unix.Renameat2(int(p.state.Fd()), temp, int(parent.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if p.afterRename != nil {
		if err := p.afterRename(); err != nil {
			return err
		}
	}
	return errors.Join(prepared.Sync(), parent.Sync(), p.state.Sync())
}

func (p *Publisher) removeDirectory(ctx context.Context, e journal.Entry, parent *os.File, name string, visible *os.File, base *journal.Version, absent bool) error {
	if base == nil || !base.Directory {
		return fmt.Errorf("explicit directory base required")
	}
	if absent {
		started, err := p.started(e.Next.ID)
		if err != nil {
			return err
		}
		if !started {
			return ErrExternal
		}
		return errors.Join(parent.Sync(), p.state.Sync())
	}
	prior, err := p.directoryReceipt(base.ID)
	if err != nil {
		return err
	}
	current, err := identity(visible)
	if err != nil {
		return err
	}
	if prior != current {
		return ErrExternal
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.checkParent(e.Path, parent); err != nil {
		return err
	}
	if err := p.markStarted(e.Next.ID); err != nil {
		return err
	}
	if p.beforeRename != nil {
		if err := p.beforeRename(); err != nil {
			return err
		}
	}
	// AT_REMOVEDIR is atomic and refuses nonempty directories, including an
	// excluded child or a child created after the identity check. No recursion.
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	if p.afterRename != nil {
		if err := p.afterRename(); err != nil {
			return err
		}
	}
	return errors.Join(parent.Sync(), p.state.Sync())
}
