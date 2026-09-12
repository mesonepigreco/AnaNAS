package stage

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// RequireBudgetBootstrap optionally permits an existing observer index. Only
// its exact metadata is inspected; caller retains the index lock and must not
// interpret this check as validation of its outbox or replica history.
// It reads at most two/three names in the private native state directory, never
// a visible root. Its separate descriptor preserves other enumeration positions.
func (s *Store) RequireBudgetBootstrap(replica bool) error {
	fd, err := unix.Openat(s.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "private-state")
	defer f.Close()
	limit := 2
	if replica {
		limit = 3
	}
	names, err := f.Readdirnames(limit)
	if err != nil && err != io.EOF {
		return err
	}
	journal := false
	for _, name := range names {
		if name == "journal.db" {
			journal = true
			continue
		}
		if !replica || name != "index.db" {
			return fmt.Errorf("budget initialization found unaccounted state")
		}
		var st unix.Stat_t
		if err := unix.Fstatat(s.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
			return fmt.Errorf("invalid observer index metadata")
		}
	}
	if !journal {
		return fmt.Errorf("budget initialization requires its journal")
	}
	return nil
}
