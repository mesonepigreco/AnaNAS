// Package stage holds verified transfer candidates in a private, flat local
// directory outside both sync roots. It never publishes to a browsable sync path.
// The scheduler owns quotas, operation IDs, coordination, journal and retention.
package stage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
)

type Store struct{ fd int }

// Open requires an existing directory owned exclusively by the effective user.
// Every path component is opened without following symlinks, using openat (also
// available on the prepared NAS's older kernel). Network/FUSE stores are refused:
// a NAS receiver must execute on the NAS and use its native canonical path.
// The caller must verify this path is outside its configured sync roots.
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("staging path must be a clean absolute directory")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	fail := func(err error) (*Store, error) { unix.Close(fd); return nil, err }
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fail(err)
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0700 {
		return fail(fmt.Errorf("staging directory must be owned by the effective user with mode 0700"))
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return fail(err)
	}
	switch uint64(uint32(fs.Type)) {
	case unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.SMB_SUPER_MAGIC, unix.NFS_SUPER_MAGIC, unix.FUSE_SUPER_MAGIC:
		return fail(fmt.Errorf("staging requires a native local filesystem"))
	}
	if fs.Flags&unix.ST_RDONLY != 0 {
		return fail(fmt.Errorf("staging filesystem is read-only"))
	}
	return &Store{fd: fd}, nil
}

func (s *Store) Close() error { return unix.Close(s.fd) }

// SameDirectory compares pinned native directories without walking either path.
func (s *Store) SameDirectory(other *Store) (bool, error) {
	if other == nil {
		return false, fmt.Errorf("staging store required")
	}
	var a, b unix.Stat_t
	if err := unix.Fstat(s.fd, &a); err != nil {
		return false, err
	}
	if err := unix.Fstat(other.fd, &b); err != nil {
		return false, err
	}
	return a.Dev == b.Dev && a.Ino == b.Ino, nil
}

// OpenJournalFile opens the companion coordinator database through the pinned
// private directory, without symlink traversal. It does not take a lock: the
// database engine must exclusively lock this handle for its entire lifetime.
func (s *Store) OpenJournalFile() (*os.File, error) {
	fd, err := unix.Openat(s.fd, "journal.db", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "journal.db")
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		f.Close()
		return nil, fmt.Errorf("invalid journal file type, owner, mode or link count")
	}
	return f, nil
}

// Sync persists directory-entry changes on the configured native filesystem.
func (s *Store) Sync() error { return unix.Fsync(s.fd) }

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// RequireVacant checks only an exact candidate's two names. A coordinator uses
// it before durably claiming a new upload so existing unowned artifacts cannot
// be adopted. It does not reserve names; its caller must hold exclusive ownership.
func (s *Store) RequireVacant(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid staging operation ID")
	}
	for _, suffix := range []string{".partial", ".ready"} {
		var st unix.Stat_t
		err := unix.Fstatat(s.fd, id+suffix, &st, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil {
			return os.ErrExist
		}
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return nil
}

// Receive creates a new operation's partial file exclusively. A verified,
// flushed result becomes ID.ready without replacing any existing object. Error
// return never grants publication permission, even if a ready file exists after
// a late fsync error. Recovery must revalidate that object and the transaction.
// A killed process may leave ID.partial; it is never opened as a ready candidate.
// The receiver-local base must be immutable for this operation; Apply verifies
// every reused range. Callers must bound total staged bytes and pace/deadline I/O.
func (s *Store) Receive(ctx context.Context, id string, wire io.Reader, base io.ReaderAt, baseSize int64, baseDigest hash.Digest) (stats delta.Stats, err error) {
	err = s.receive(ctx, id, func(f *os.File) error {
		var err error
		stats, err = delta.Apply(ctx, wire, base, baseSize, baseDigest, f)
		return err
	})
	return stats, err
}

// ReceiveDeltaVerified compares the decoder's complete verified result with
// approved target metadata before sealing. It reuses the decoder's whole digest
// rather than hashing every reconstructed byte again through a second writer.
func (s *Store) ReceiveDeltaVerified(ctx context.Context, id string, wire io.Reader, base io.ReaderAt, baseSize int64, baseDigest hash.Digest, targetSize int64, targetDigest hash.Digest) (stats delta.Stats, err error) {
	if targetSize < 0 || targetSize > 8<<30 {
		return stats, fmt.Errorf("bounded target size required")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	reader := bufio.NewReaderSize(wire, 4096)
	header, err := reader.Peek(delta.HeaderBytes)
	if err != nil {
		return stats, err
	}
	h, err := delta.InspectHeader(header)
	if err != nil || h.TargetSize != targetSize {
		return stats, fmt.Errorf("delta header differs from approved target size")
	}
	err = s.receive(ctx, id, func(f *os.File) error {
		var err error
		stats, err = delta.Apply(ctx, reader, base, baseSize, baseDigest, f)
		if err != nil {
			return err
		}
		if stats.LiteralBytes+stats.ReusedBytes != targetSize || stats.Digest != targetDigest {
			return fmt.Errorf("decoded candidate differs from approved target")
		}
		return nil
	})
	return stats, err
}

func (s *Store) receive(ctx context.Context, id string, produce func(*os.File) error) (err error) {
	return s.receiveKind(ctx, id, "", produce)
}

func (s *Store) receiveKind(ctx context.Context, id, kind string, produce func(*os.File) error) (err error) {
	if !validID(id) {
		return fmt.Errorf("invalid staging operation ID")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	partial, ready := id+kind+".partial", id+kind+".ready"
	// Avoid consuming a duplicate transfer. Linkat below remains the final
	// exclusive creation check if another receiver races this metadata check.
	var st unix.Stat_t
	if err := unix.Fstatat(s.fd, ready, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	fd, err := unix.Openat(s.fd, partial, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), partial)
	defer func() {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		// Only this call's O_EXCL-created entry is removed, never other IDs.
		if unlinkErr := unix.Unlinkat(s.fd, partial, 0); unlinkErr != nil && !errors.Is(unlinkErr, unix.ENOENT) {
			err = errors.Join(err, unlinkErr)
		}
		err = errors.Join(err, unix.Fsync(s.fd))
	}()
	err = produce(f)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = f.Chmod(0400); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	err = f.Close()
	f = nil
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// linkat is atomic and cannot overwrite an existing name. The subsequent
	// unlink + directory fsync completes durable creation on supported storage.
	if err = unix.Linkat(s.fd, partial, s.fd, ready, 0); err != nil {
		return err
	}
	return nil
}

// OpenReady returns a read-only regular object, never an interrupted partial.
// The caller must compare its digest/length with its durable transaction record
// after a restart; the .ready suffix alone is not a recovery proof.
func (s *Store) OpenReady(id string) (*os.File, error) {
	return s.openReadyKind(id, "")
}

func (s *Store) openReadyKind(id, kind string) (*os.File, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid staging operation ID")
	}
	fd, err := unix.Openat(s.fd, id+kind+".ready", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), id+kind+".ready")
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0400 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		f.Close()
		return nil, fmt.Errorf("invalid ready object type, owner, mode or link count")
	}
	return f, nil
}

// DiscardPartial is explicit recovery for a known, abandoned operation. The
// coordinator must first prove no live receiver owns it. There is no directory
// sweep or age-based lock stealing. Ready-object abort is a separate operation
// requiring durable proof that an upload preparation never became sendable.
func (s *Store) DiscardPartial(id string) error {
	return s.discardPartialKind(id, "")
}

func (s *Store) discardPartialKind(id, kind string) error {
	if !validID(id) {
		return fmt.Errorf("invalid staging operation ID")
	}
	if err := unix.Unlinkat(s.fd, id+kind+".partial", 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(s.fd)
}
