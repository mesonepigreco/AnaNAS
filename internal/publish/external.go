package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
	"nas-sync/internal/content"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

// HashExternal compares one selected observed file with its journal head. It
// uses the native NAS-compatible path checks and paced reads, never a tree scan.
func (p *Publisher) HashExternal(ctx context.Context, path string, want index.Fingerprint) (hash.Digest, error) {
	parent, name, err := p.parent(path)
	if err != nil {
		return hash.Digest{}, err
	}
	defer parent.Close()
	f, err := openRegular(parent, name)
	if err != nil {
		return hash.Digest{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return hash.Digest{}, err
	}
	if content.Fingerprint(info) != want {
		return hash.Digest{}, ErrExternal
	}
	mid, err := mountID(f)
	if err != nil || mid != p.mount {
		return hash.Digest{}, fmt.Errorf("external file crosses native mount")
	}
	before, err := stat(f)
	if err != nil {
		return hash.Digest{}, err
	}
	digest, _, err := p.digestCopy(ctx, f, nil)
	if err != nil {
		return hash.Digest{}, err
	}
	if err := p.checkParent(path, parent); err != nil {
		return hash.Digest{}, err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return hash.Digest{}, err
	}
	if !sameFingerprint(before, named) {
		return hash.Digest{}, ErrExternal
	}
	return digest, nil
}

// CaptureExternal retains a regular file, directory identity or confirmed leaf
// absence from a native writer such as SMB. Call only inside ImportExternal,
// after reserving this exact ID and size in the same native staging directory.
// The visible path is never modified.
// The compatible openat/mount/parent checks are shared with native publication.
func (p *Publisher) CaptureExternal(ctx context.Context, e journal.Entry, want *index.Fingerprint, store *stage.Store, resume bool) (journal.Version, error) {
	v := e.Next
	if store == nil || v.Size < 0 || v.Size > 8<<30 {
		return v, fmt.Errorf("bounded file/directory external capture required")
	}
	if err := ctx.Err(); err != nil {
		return v, err
	}
	if v.Tombstone {
		return p.captureExternalAbsence(ctx, e, want, resume)
	}
	if v.Directory {
		return p.captureExternalDirectory(ctx, e, want, resume)
	}
	if resume {
		if err := store.DiscardPartial(v.ID); err != nil {
			return v, err
		}
		f, err := store.OpenReady(v.ID)
		if err == nil {
			defer f.Close()
			st, err := f.Stat()
			if err != nil {
				return v, err
			}
			if st.Size() != v.Size {
				return v, ErrExternal
			}
			d, _, err := p.digestCopy(ctx, f, nil)
			v.Digest = d
			return v, err
		}
		if !errors.Is(err, os.ErrNotExist) {
			return v, err
		}
	}
	if want == nil {
		return v, fmt.Errorf("fresh observation required when no retained snapshot exists")
	}
	parent, name, err := p.parent(e.Path)
	if err != nil {
		return v, err
	}
	defer parent.Close()
	f, err := openRegular(parent, name)
	if err != nil {
		return v, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return v, err
	}
	if content.Fingerprint(info) != *want || info.Size() != v.Size {
		return v, ErrExternal
	}
	mid, err := mountID(f)
	if err != nil || mid != p.mount {
		return v, fmt.Errorf("external file crosses native mount")
	}
	before, err := stat(f)
	if err != nil {
		return v, err
	}
	digest, err := store.Capture(ctx, v.ID, v.Size, func(out io.Writer) error {
		if _, _, err := p.digestCopy(ctx, f, out); err != nil {
			return err
		}
		if err := p.checkParent(e.Path, parent); err != nil {
			return err
		}
		var named unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if !sameFingerprint(before, named) {
			return ErrExternal
		}
		return nil
	})
	v.Digest = digest
	return v, err
}
