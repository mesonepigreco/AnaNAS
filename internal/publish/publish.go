// Package publish applies prepared journal records on a native filesystem.
// It retains displaced inodes, never updates visible content in place, and
// requires the coordinator's exclusive ownership around every call.
package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zeebo/blake3"
	"golang.org/x/sys/unix"
	"nas-sync/internal/exclude"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

var ErrExternal = errors.New("visible content changed outside the prepared transaction; retained candidates require resolution")

type Publisher struct {
	root, state *os.File
	rootPath    string
	mount       string
	lookup      func(string) (*journal.Version, error)
	exclusions  *exclude.Matcher
	rate        int64
	// Used only by local crash/race tests at actual filesystem boundaries.
	afterRename  func() error
	beforeRename func() error
}

func openAbsolute(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("expected clean absolute directory")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(path[1:], "/") {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}
func stat(f *os.File) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstat(int(f.Fd()), &st)
	return st, err
}

func sameFingerprint(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Size == b.Size && a.Mode == b.Mode && a.Nlink == b.Nlink && a.Uid == b.Uid && a.Gid == b.Gid && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}
func mountID(f *os.File) (string, error) {
	// fdinfo is local kernel metadata. Require mnt_id to distinguish bind mounts
	// sharing a device without statx/openat2 or any NAS payload read. Availability
	// still needs validation on the prepared NAS runtime.
	p, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(p), "\n") {
		if strings.HasPrefix(line, "mnt_id:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "mnt_id:"))
			if _, err := strconv.ParseUint(v, 10, 64); err != nil {
				return "", err
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("mount identity unavailable")
}
func native(f *os.File) error {
	var s unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &s); err != nil {
		return err
	}
	switch uint64(uint32(s.Type)) {
	case unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.SMB_SUPER_MAGIC, unix.NFS_SUPER_MAGIC, unix.FUSE_SUPER_MAGIC:
		return fmt.Errorf("publisher requires native local storage")
	}
	if s.Flags&unix.ST_RDONLY != 0 {
		return fmt.Errorf("publisher filesystem is read-only")
	}
	return nil
}

// Open requires separate canonical roots on the same native mount. The private
// state directory must be owned by the effective user with mode 0700. No files
// are created here. Every publication still requires a live journal owner and
// validated transfer/LAN policy; opening this object grants neither.
func Open(rootPath, statePath string, lookup func(string) (*journal.Version, error), patterns []string, rate int64) (p *Publisher, err error) {
	if lookup == nil || rate < 1024 || rate > 1<<30 {
		return nil, fmt.Errorf("version lookup and bounded read rate required")
	}
	if rootPath == statePath || strings.HasPrefix(rootPath, statePath+"/") || strings.HasPrefix(statePath, rootPath+"/") {
		return nil, fmt.Errorf("sync and state roots must be disjoint")
	}
	m, err := exclude.Compile(patterns)
	if err != nil {
		return nil, err
	}
	r, err := openAbsolute(rootPath)
	if err != nil {
		return nil, err
	}
	s, err := openAbsolute(statePath)
	if err != nil {
		r.Close()
		return nil, err
	}
	defer func() {
		if err != nil {
			r.Close()
			s.Close()
		}
	}()
	if err = native(r); err != nil {
		return nil, err
	}
	if err = native(s); err != nil {
		return nil, err
	}
	rs, err := stat(r)
	if err != nil {
		return nil, err
	}
	ss, err := stat(s)
	if err != nil {
		return nil, err
	}
	if ss.Uid != uint32(os.Geteuid()) || ss.Mode&0777 != 0700 || rs.Dev != ss.Dev {
		return nil, fmt.Errorf("unsafe state ownership, permissions or filesystem")
	}
	rm, err := mountID(r)
	if err != nil {
		return nil, err
	}
	sm, err := mountID(s)
	if err != nil {
		return nil, err
	}
	if rm != sm {
		return nil, fmt.Errorf("sync and state roots must share one mount")
	}
	return &Publisher{root: r, state: s, rootPath: rootPath, mount: rm, lookup: lookup, exclusions: m, rate: rate}, nil
}
func (p *Publisher) Close() error { return errors.Join(p.root.Close(), p.state.Close()) }

func (p *Publisher) parent(path string) (*os.File, string, error) {
	if path == "." || len(path) > 4096 || !fs.ValidPath(path) || strings.ContainsAny(path, "\\\x00") || p.exclusions.Match(path, false) {
		return nil, "", fmt.Errorf("invalid or excluded publication path")
	}
	// Verify the configured root still names the pinned directory.
	r, err := openAbsolute(p.rootPath)
	if err != nil {
		return nil, "", err
	}
	a, ae := stat(r)
	b, be := stat(p.root)
	rm, me := mountID(r)
	r.Close()
	if ae != nil || be != nil {
		return nil, "", errors.Join(ae, be)
	}
	if a.Dev != b.Dev || a.Ino != b.Ino {
		return nil, "", fmt.Errorf("publication root replaced")
	}
	if me != nil || rm != p.mount {
		return nil, "", fmt.Errorf("publication root mount changed")
	}
	fd, err := unix.Openat(int(p.root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(path, "/")
	for i, part := range parts[:len(parts)-1] {
		if p.exclusions.Match(strings.Join(parts[:i+1], "/"), true) {
			unix.Close(fd)
			return nil, "", fmt.Errorf("publication parent excluded")
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, "", err
		}
		fd = next
		f := os.NewFile(uintptr(fd), part)
		mid, err := mountID(f)
		if err != nil || mid != p.mount {
			f.Close()
			if err != nil {
				return nil, "", err
			}
			return nil, "", fmt.Errorf("publication crosses mount")
		}
		// Keep the owned file handle across iterations; releasing via raw fd
		// would let its finalizer close a recycled descriptor.
		if i < len(parts)-2 {
			dup, err := unix.Dup(fd)
			f.Close()
			if err != nil {
				return nil, "", err
			}
			unix.CloseOnExec(dup)
			fd = dup
		} else {
			return f, parts[len(parts)-1], nil
		}
	}
	return os.NewFile(uintptr(fd), "publication parent"), parts[len(parts)-1], nil
}

func openRegular(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	st, err := stat(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, fmt.Errorf("publication object is not regular")
	}
	return f, nil
}

// digestCopy hashes one stable file with a reusable 64 KiB buffer and optional
// local staging output. A transfer policy supplies rate; no content work runs
// on an idle timer. Caller validates the descriptor's mount before calling.
func (p *Publisher) digestCopy(ctx context.Context, f *os.File, w io.Writer) (hash.Digest, int64, error) {
	var zero hash.Digest
	before, err := stat(f)
	if err != nil {
		return zero, 0, err
	}
	if before.Size < 0 || before.Size > int64(hash.MaxBlockSize)*manifest.MaxBlocks {
		return zero, 0, fmt.Errorf("publication content too large")
	}
	if _, err := f.Seek(0, 0); err != nil {
		return zero, 0, err
	}
	buffer := make([]byte, 64*1024)
	h := blake3.New()
	var total int64
	var next time.Time
	for {
		if err := ctx.Err(); err != nil {
			return zero, total, err
		}
		if wait := time.Until(next); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return zero, total, ctx.Err()
			case <-timer.C:
			}
		}
		n, err := f.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > before.Size {
				return zero, total, ErrExternal
			}
			h.Write(buffer[:n])
			if w != nil {
				written, e := w.Write(buffer[:n])
				if e != nil {
					return zero, total, e
				}
				if written != n {
					return zero, total, io.ErrShortWrite
				}
			}
			next = time.Now().Add(time.Duration(int64(n) * int64(time.Second) / p.rate))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return zero, total, err
		}
	}
	after, err := stat(f)
	if err != nil {
		return zero, total, err
	}
	if !sameFingerprint(before, after) || total != before.Size {
		return zero, total, ErrExternal
	}
	var d hash.Digest
	h.Sum(d[:0])
	return d, total, nil
}

func (p *Publisher) checkFile(ctx context.Context, dir *os.File, name string) (*os.File, hash.Digest, int64, error) {
	f, err := openRegular(dir, name)
	if err != nil {
		return nil, hash.Digest{}, 0, err
	}
	m, err := mountID(f)
	if err != nil || m != p.mount {
		f.Close()
		return nil, hash.Digest{}, 0, fmt.Errorf("publication file mount differs")
	}
	d, n, err := p.digestCopy(ctx, f, nil)
	if err != nil {
		f.Close()
		return nil, d, n, err
	}
	return f, d, n, nil
}
func same(d hash.Digest, n int64, v *journal.Version) bool {
	return v != nil && !v.Tombstone && !v.Directory && v.Size == n && v.Digest == d
}

func (p *Publisher) ready(id string) (*os.File, error) {
	f, err := openRegular(p.state, id+".ready")
	if err != nil {
		return nil, err
	}
	s, err := stat(f)
	m, me := mountID(f)
	if err != nil || me != nil || m != p.mount || s.Mode&0777 != 0400 || s.Nlink != 1 || s.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, fmt.Errorf("invalid immutable candidate")
	}
	return f, nil
}

func (p *Publisher) checkParent(path string, parent *os.File) error {
	current, _, err := p.parent(path)
	if err != nil {
		return err
	}
	defer current.Close()
	a, ae := stat(parent)
	b, be := stat(current)
	if ae != nil || be != nil {
		return errors.Join(ae, be)
	}
	if a.Dev != b.Dev || a.Ino != b.Ino {
		return ErrExternal
	}
	return nil
}

func (p *Publisher) started(id string) (bool, error) {
	f, err := openRegular(p.state, id+".started")
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	s, err := stat(f)
	if err != nil {
		return false, err
	}
	if s.Size != 0 || s.Mode&0777 != 0400 || s.Uid != uint32(os.Geteuid()) || s.Nlink != 1 {
		return false, fmt.Errorf("invalid publication intent marker")
	}
	return true, nil
}
func (p *Publisher) markStarted(id string) error {
	if exists, err := p.started(id); err != nil || exists {
		return err
	}
	fd, err := unix.Openat(int(p.state.Fd()), id+".started", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0400)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), id+".started")
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr, p.state.Sync())
}

// Publish applies only records prepared by the journal, one path at a time.
// Parents must be published before children; directory deletion accepts only
// empty directories. Every displaced file remains in private storage. Atomic exchange
// retains a racing external replacement and reports conflict after exchange,
// possibly leaving the incoming version visible until explicit resolution.
// This is not CAS against noncooperating applications or multi-file atomicity.
func (p *Publisher) Publish(ctx context.Context, r journal.Record) error {
	if r.Committed || r.Sequence == 0 || r.Epoch == 0 || len(r.Entries) < 1 || len(r.Entries) > journal.MaxEntries {
		return fmt.Errorf("prepared record required")
	}
	for _, e := range r.Entries {
		if !manifest.ValidID(e.Next.ID) {
			return fmt.Errorf("invalid candidate identity")
		}
		if err := p.apply(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) apply(ctx context.Context, e journal.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, name, err := p.parent(e.Path)
	if err != nil {
		return err
	}
	defer parent.Close()
	var base *journal.Version
	if e.Expected != "" {
		base, err = p.lookup(e.Expected)
		if err != nil {
			return err
		}
	}
	if e.Next.Directory || base != nil && base.Directory {
		return p.directory(ctx, e, parent, name, base)
	}
	visible, d, n, err := p.checkFile(ctx, parent, name)
	absent := errors.Is(err, unix.ENOENT)
	if err != nil && !absent {
		return err
	}
	if visible != nil {
		defer visible.Close()
	}
	if same(d, n, &e.Next) && !absent {
		// Idempotent replay after namespace publication but before journal commit.
		// A displaced unexpected version remains a conflict across every retry.
		old, od, on, oldErr := p.checkFile(ctx, p.state, e.Next.ID+".publish")
		if oldErr == nil {
			defer old.Close()
			if !same(od, on, base) && !same(od, on, &e.Next) {
				return ErrExternal
			}
			if err := old.Sync(); err != nil {
				return err
			}
		} else if !errors.Is(oldErr, unix.ENOENT) {
			return oldErr
		}
		ready, err := p.ready(e.Next.ID)
		if err != nil {
			return err
		}
		defer ready.Close()
		rd, rn, err := p.digestCopy(ctx, ready, nil)
		if err != nil {
			return err
		}
		if !same(rd, rn, &e.Next) {
			return fmt.Errorf("immutable candidate verification failed")
		}
		return errors.Join(visible.Sync(), parent.Sync(), p.state.Sync())
	}
	if e.Next.Tombstone {
		return p.remove(ctx, e, parent, name, visible, d, n, base, absent)
	}
	if absent && (base != nil && !base.Tombstone) || !absent && !same(d, n, base) {
		return ErrExternal
	}
	ready, err := p.ready(e.Next.ID)
	if err != nil {
		return err
	}
	defer ready.Close()
	temp := e.Next.ID + ".publish"
	var existing unix.Stat_t
	err = unix.Fstatat(int(p.state.Fd()), temp, &existing, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		// A pre-rename crash may leave the complete new copy. Anything else is
		// retained for resolution, never truncated as an automatic retry.
		f, td, tn, checkErr := p.checkFile(ctx, p.state, temp)
		if checkErr != nil {
			return checkErr
		}
		defer f.Close()
		st, err := stat(f)
		if err != nil {
			return err
		}
		if st.Mode&0777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
			return fmt.Errorf("unsafe prepared copy")
		}
		if !same(td, tn, &e.Next) {
			return ErrExternal
		}
		if err := f.Sync(); err != nil {
			return err
		}
	} else {
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
		started, err := p.started(e.Next.ID)
		if err != nil {
			return err
		}
		if started {
			return ErrExternal
		} // a consumed copy must not be silently recreated
		copying := e.Next.ID + ".copying"
		// This suffix is never moved into the visible tree. Under exclusive
		// coordinator ownership a killed copy can be discarded by exact ID.
		if err := unix.Unlinkat(int(p.state.Fd()), copying, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		fd, err := unix.Openat(int(p.state.Fd()), copying, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), copying)
		td, tn, copyErr := p.digestCopy(ctx, ready, f)
		if copyErr == nil && !same(td, tn, &e.Next) {
			copyErr = fmt.Errorf("immutable candidate verification failed")
		}
		if copyErr == nil {
			copyErr = f.Sync()
		}
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			// This call owns the never-published exclusive temporary. Failed
			// copies can be removed; retained ready/displaced objects are untouched.
			cleanup := unix.Unlinkat(int(p.state.Fd()), copying, 0)
			return errors.Join(copyErr, closeErr, cleanup, p.state.Sync())
		}
		if err := unix.Renameat2(int(p.state.Fd()), copying, int(p.state.Fd()), temp, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err := p.state.Sync(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Recheck that this parent still belongs to the configured root.
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
	flags := uint(unix.RENAME_NOREPLACE)
	if !absent {
		flags = unix.RENAME_EXCHANGE
	}
	if err := unix.Renameat2(int(p.state.Fd()), temp, int(parent.Fd()), name, flags); err != nil {
		return err
	}
	if p.afterRename != nil {
		if err := p.afterRename(); err != nil {
			return err
		}
	}
	if err := errors.Join(parent.Sync(), p.state.Sync()); err != nil {
		return err
	}
	if !absent {
		old, od, on, err := p.checkFile(ctx, p.state, temp)
		if err != nil {
			return err
		}
		defer old.Close()
		if !same(od, on, base) {
			return ErrExternal
		}
		if err := old.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) remove(ctx context.Context, e journal.Entry, parent *os.File, name string, visible *os.File, d hash.Digest, n int64, base *journal.Version, absent bool) error {
	displaced := e.Next.ID + ".displaced"
	if absent {
		if base == nil || base.Tombstone {
			return errors.Join(parent.Sync(), p.state.Sync())
		}
		old, od, on, err := p.checkFile(ctx, p.state, displaced)
		if err != nil {
			return err
		}
		defer old.Close()
		if !same(od, on, base) {
			return ErrExternal
		}
		return errors.Join(old.Sync(), parent.Sync(), p.state.Sync())
	}
	if !same(d, n, base) {
		return ErrExternal
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
	if err := unix.Renameat2(int(parent.Fd()), name, int(p.state.Fd()), displaced, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if p.afterRename != nil {
		if err := p.afterRename(); err != nil {
			return err
		}
	}
	if err := errors.Join(parent.Sync(), p.state.Sync()); err != nil {
		return err
	}
	old, od, on, err := p.checkFile(ctx, p.state, displaced)
	if err != nil {
		return err
	}
	defer old.Close()
	if !same(od, on, base) {
		return ErrExternal
	}
	return old.Sync()
}
