package content

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
	"nas-sync/internal/index"
)

var ErrUnsupported = errors.New("unsupported or excluded candidate")

// CheckAvailable distinguishes a missing/replaced root from a single bad file.
func (r *Root) CheckAvailable() error { return r.checkRoot() }

// SameObservedContent compares regular files strictly. Directories synchronize
// identity/type, not size or timestamps changed by independent child operations.
// The caller must still validate native root/path access and index generation.
func SameObservedContent(a, b index.Fingerprint) bool {
	if os.FileMode(a.Mode).IsDir() && os.FileMode(b.Mode).IsDir() {
		return a.Device == b.Device && a.Inode == b.Inode
	}
	return a == b
}

// Metadata checks one exact native path without reading content or enumerating
// a directory. Absence is accepted only for a missing leaf beneath a still
// accessible, unchanged parent and root. Symlinks, child mounts and special files
// are refused; an inaccessible/missing parent is never evidence of deletion.
func (r *Root) Metadata(name string) (fingerprint index.Fingerprint, missing bool, err error) {
	if len(name) > 4096 || name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
		return fingerprint, false, fmt.Errorf("invalid candidate path")
	}
	if r.matcher.Match(name, false) {
		return fingerprint, false, fmt.Errorf("candidate is excluded")
	}
	if err := r.checkRoot(); err != nil {
		return fingerprint, false, err
	}
	directory, leaf := path.Split(name)
	directory = strings.TrimSuffix(directory, "/")
	if directory == "" {
		directory = "."
	}
	how := &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV}
	parent, err := unix.Openat2(r.fd, directory, how)
	if err != nil {
		return fingerprint, false, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat2(parent, leaf, &unix.OpenHow{Flags: unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if errors.Is(err, unix.ENOENT) {
		missing = true
	} else if err != nil {
		return fingerprint, false, err
	} else {
		f := os.NewFile(uintptr(fd), name)
		info, statErr := f.Stat()
		closeErr := f.Close()
		if err := errors.Join(statErr, closeErr); err != nil {
			return fingerprint, false, err
		}
		if (!info.Mode().IsRegular() && !info.IsDir()) || r.matcher.Match(name, info.IsDir()) {
			return fingerprint, false, ErrUnsupported
		}
		fingerprint = Fingerprint(info)
	}
	current, err := unix.Openat2(r.fd, directory, how)
	if err != nil {
		return fingerprint, false, err
	}
	defer unix.Close(current)
	var before, after unix.Stat_t
	if err := unix.Fstat(parent, &before); err != nil {
		return fingerprint, false, err
	}
	if err := unix.Fstat(current, &after); err != nil {
		return fingerprint, false, err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino {
		return fingerprint, false, ErrChanged
	}
	if err := r.checkRoot(); err != nil {
		return fingerprint, false, err
	}
	return fingerprint, missing, nil
}
