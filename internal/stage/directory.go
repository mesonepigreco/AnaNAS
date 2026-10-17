package stage

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// CaptureDirectoryIdentity retains the source inode of a reserved upload before
// it can be transmitted. The caller verifies the observed source before and
// after capture. The format is shared with the native publisher's receipts.
func (s *Store) CaptureDirectoryIdentity(id string, device, inode uint64) error {
	if !validID(id) || inode == 0 {
		return fmt.Errorf("valid directory upload identity required")
	}
	temp, name := id+".directory-writing", id+".directory"
	fd, err := unix.Openat(s.fd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0400)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), temp)
	var data [16]byte
	binary.BigEndian.PutUint64(data[:8], device)
	binary.BigEndian.PutUint64(data[8:], inode)
	n, err := f.Write(data[:])
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := unix.Renameat2(s.fd, temp, s.fd, name, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return s.Sync()
}
