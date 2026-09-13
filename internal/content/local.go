// Package content provides bounded content reads for the transfer scheduler.
// It is deliberately separate from the metadata-only observer. Local roots and
// candidates reject network/FUSE filesystems, symlinks, child mounts and special
// files; no NAS payload is downloaded to compute a manifest through this API.
package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"nas-sync/internal/diff"
	"nas-sync/internal/exclude"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/manifest"
)

var ErrChanged = errors.New("local candidate changed; compare again")

type Root struct {
	fd      int
	path    string
	matcher *exclude.Matcher
}

func OpenRoot(path string, patterns []string) (*Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("content root must be absolute")
	}
	m, err := exclude.Compile(patterns)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Statfs_t
	if err = unix.Fstatfs(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	switch uint64(uint32(st.Type)) {
	case unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.SMB_SUPER_MAGIC, unix.NFS_SUPER_MAGIC, unix.FUSE_SUPER_MAGIC:
		unix.Close(fd)
		return nil, fmt.Errorf("content root must be local, not a network/FUSE mount")
	}
	return &Root{fd: fd, path: path, matcher: m}, nil
}

func (r *Root) Close() error { return unix.Close(r.fd) }

func Fingerprint(info os.FileInfo) index.Fingerprint {
	st := info.Sys().(*syscall.Stat_t)
	return index.Fingerprint{Device: uint64(st.Dev), Inode: st.Ino, Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), CtimeNS: st.Ctim.Nano(), Mode: uint32(info.Mode())}
}

func (r *Root) checkRoot() error {
	fd, err := unix.Open(r.path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var a, b unix.Stat_t
	if err := unix.Fstat(fd, &a); err != nil {
		return err
	}
	if err := unix.Fstat(r.fd, &b); err != nil {
		return err
	}
	if a.Dev != b.Dev || a.Ino != b.Ino {
		return ErrChanged
	}
	return nil
}

func (r *Root) open(path string) (*os.File, error) {
	if len(path) > 4096 || path == "." || !fs.ValidPath(path) || strings.ContainsAny(path, "\\\x00") {
		return nil, fmt.Errorf("invalid candidate path")
	}
	if r.matcher.Match(path, false) {
		return nil, fmt.Errorf("candidate is excluded")
	}
	// O_NONBLOCK prevents opening a FIFO from hanging before its type check.
	fd, err := unix.Openat2(r.fd, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, ErrUnsupported
	}
	return f, nil
}

func matches(f *os.File, want index.Fingerprint) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if Fingerprint(st) != want {
		return ErrChanged
	}
	return nil
}

// Verify rechecks both the root identity and the name's current fingerprint.
// Call immediately before publication as well as after hashing. A filesystem
// check is not an atomic CAS against arbitrary applications writing the file.
func (r *Root) Verify(path string, want index.Fingerprint) error {
	if err := r.checkRoot(); err != nil {
		return err
	}
	f, err := r.open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return matches(f, want)
}

// Hash builds a stable, bounded manifest at the configured read rate. It allows
// one block of initial burst, checks cancellation between reads, and rejects
// oversized candidates before opening their content. The scheduler must also
// compare the index generation and enforce pause/LAN gates around this work.
func (r *Root) Hash(ctx context.Context, path string, want index.Fingerprint, blockSize int, bytesPerSecond int64) (*manifest.Manifest, error) {
	if blockSize < 1024 || blockSize > hash.MaxBlockSize || blockSize&(blockSize-1) != 0 {
		return nil, fmt.Errorf("invalid block size")
	}
	if bytesPerSecond < 1024 || bytesPerSecond > 1<<30 {
		return nil, fmt.Errorf("read rate out of range")
	}
	if want.Size < 0 || want.Size > int64(blockSize)*manifest.MaxBlocks {
		return nil, fmt.Errorf("candidate exceeds %d-block manifest limit", manifest.MaxBlocks)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.checkRoot(); err != nil {
		return nil, err
	}
	f, err := r.open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := matches(f, want); err != nil {
		return nil, err
	}
	id, err := hash.RandomDigest()
	if err != nil {
		return nil, err
	}
	m := &manifest.Manifest{BlockSize: blockSize, Content: diff.Manifest{ID: id.Hex(), Size: want.Size,
		Blocks: make([]diff.Block, 0, int((want.Size+int64(blockSize)-1)/int64(blockSize)))}}
	reader := &pacedReader{ctx: ctx, r: io.LimitReader(f, want.Size+1), rate: bytesPerSecond}
	err = hash.Stream(ctx, reader, int64(blockSize), func(b hash.Block) error {
		if len(m.Content.Blocks) >= manifest.MaxBlocks {
			return ErrChanged
		}
		m.Content.Blocks = append(m.Content.Blocks, diff.Block{Digest: b.Digest, Size: b.Size})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := matches(f, want); err != nil {
		return nil, err
	}
	if err := r.Verify(path, want); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChanged, err)
	}
	return m, nil
}

type pacedReader struct {
	ctx  context.Context
	r    io.Reader
	rate int64
	next time.Time
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if delay := time.Until(r.next); delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-t.C:
		}
	}
	n, err := r.r.Read(p)
	r.next = time.Now().Add(time.Duration(int64(n) * int64(time.Second) / r.rate))
	return n, err
}

// Candidate owns one open file and an immutable copy of its bounded manifest.
// A transfer reuses this handle and buffer across blocks instead of validating
// an entire manifest or opening a file for every range.
type Candidate struct {
	root      *Root
	file      *os.File
	path      string
	want      index.Fingerprint
	blockSize int
	blocks    []diff.Block
}

func (r *Root) OpenCandidate(path string, want index.Fingerprint, m *manifest.Manifest) (*Candidate, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.Content.Tombstone || m.Content.Directory || m.Content.Size != want.Size {
		return nil, fmt.Errorf("candidate manifest does not match file size")
	}
	if err := r.checkRoot(); err != nil {
		return nil, err
	}
	f, err := r.open(path)
	if err != nil {
		return nil, err
	}
	if err := matches(f, want); err != nil {
		f.Close()
		return nil, err
	}
	return &Candidate{root: r, file: f, path: path, want: want, blockSize: m.BlockSize, blocks: append([]diff.Block(nil), m.Content.Blocks...)}, nil
}

func (c *Candidate) Close() error { return c.file.Close() }

func (c *Candidate) Verify() error {
	if err := matches(c.file, c.want); err != nil {
		return err
	}
	return c.root.Verify(c.path, c.want)
}

// ReadBlock verifies the selected range before exposing bytes to a transport.
// The caller reuses a bounded buffer, paces transfer I/O, and calls Verify and
// checks the index generation immediately before publication.
func (c *Candidate) ReadBlock(ctx context.Context, block int, buffer []byte) ([]byte, error) {
	if block < 0 || block >= len(c.blocks) {
		return nil, fmt.Errorf("invalid requested block")
	}
	b := c.blocks[block]
	if int64(len(buffer)) < b.Size {
		return nil, fmt.Errorf("block buffer is too small")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	buffer = buffer[:int(b.Size)]
	if _, err := c.file.ReadAt(buffer, int64(block)*int64(c.blockSize)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hash.SumBytes(buffer) != b.Digest {
		return nil, ErrChanged
	}
	return buffer, nil
}
