package stage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ReplicaSnapshot inspects one exact, journal-owned immutable name. Unknown
// artifacts, symlinks, hard links and altered sizes fail closed. No directory
// enumeration or visible-file access is performed.
func (s *Store) ReplicaSnapshot(id string, size int64, remove bool) (allocated int64, present bool, err error) {
	if !validID(id) || size < 0 {
		return 0, false, fmt.Errorf("invalid snapshot identity/size")
	}
	var st unix.Stat_t
	err = unix.Fstatat(s.fd, id+".ready", &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0400 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size != size {
		return 0, false, fmt.Errorf("unexpected immutable snapshot: %s", id)
	}
	if remove {
		if err = unix.Unlinkat(s.fd, id+".ready", 0); err != nil {
			return 0, false, err
		}
	}
	return st.Blocks * 512, true, nil
}
