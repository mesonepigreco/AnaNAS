package stage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// RequireUploadVacant checks the exact snapshot and encoded-spool names before
// a serialized caller records ownership. It neither creates nor lists anything.
func (s *Store) RequireUploadVacant(id string) error {
	if err := s.RequireVacant(id); err != nil {
		return err
	}
	return s.RequireWireVacant(id)
}

// RequireExternalVacant additionally reserves the native directory/absence
// receipt names. Only the local capture coordinator may claim or abort them.
func (s *Store) RequireExternalVacant(id string) error {
	if err := s.RequireUploadVacant(id); err != nil {
		return err
	}
	for _, suffix := range []string{".directory", ".directory-writing", ".absent", ".absent-writing"} {
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

// AbortExternal removes only an uncommitted local capture's snapshot and fixed
// size directory/absence receipts, after the journal proves exclusive ID ownership.
// It never touches a visible directory or credits space before its final flush.
func (s *Store) AbortExternal(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid capture ID")
	}
	for _, suffix := range []string{".directory", ".directory-writing", ".absent", ".absent-writing"} {
		var st unix.Stat_t
		err := unix.Fstatat(s.fd, id+suffix, &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0400 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size < 0 || st.Size > 16 || ((suffix == ".directory" || suffix == ".absent") && st.Size != 16) {
			return fmt.Errorf("unexpected native capture receipt")
		}
		if err := unix.Unlinkat(s.fd, id+suffix, 0); err != nil {
			return err
		}
	}
	if err := s.removeOwnedArtifacts(id, []string{"", ".wire"}); err != nil {
		return err
	}
	return s.Sync()
}

// RequireWireVacant checks only the two exact encoded-spool names.
func (s *Store) RequireWireVacant(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid staging operation ID")
	}
	for _, suffix := range []string{".wire.partial", ".wire.ready"} {
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

// AbortUncommittedUpload removes only a durably claimed, unused upload ID.
// Call under the owning index's preparation lock and the journal lifetime lock,
// after proving the ID is absent from committed/prepared history and outboxes.
// This is not retention cleanup for uploaded versions. It handles the two-link
// window between sealing a ready name and unlinking its partial alias.
func (s *Store) AbortUncommittedUpload(id string) error {
	if err := s.removeOwnedArtifacts(id, []string{"", ".wire"}); err != nil {
		return err
	}
	return s.Sync()
}

// RemoveCommittedWire requires journal proof of a committed upload and its
// reservation. It removes only encoded spools, preserving content snapshots.
// Caller owns journal/replica serialization and MUST call Sync after its bounded
// batch before crediting space. This permits one directory flush for the batch.
func (s *Store) RemoveCommittedWire(id string) error {
	return s.removeOwnedArtifacts(id, []string{".wire"})
}

func (s *Store) removeOwnedArtifacts(id string, kinds []string) error {
	if !validID(id) {
		return fmt.Errorf("invalid staging operation ID")
	}
	for _, kind := range kinds {
		names := []string{id + kind + ".partial", id + kind + ".ready"}
		var stats [2]unix.Stat_t
		var exists [2]bool
		for i, name := range names {
			err := unix.Fstatat(s.fd, name, &stats[i], unix.AT_SYMLINK_NOFOLLOW)
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			if err != nil {
				return err
			}
			exists[i] = true
			st := stats[i]
			mode := st.Mode & 0777
			if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || (mode != 0400 && mode != 0600) || (i == 1 && mode != 0400) || st.Nlink < 1 || st.Nlink > 2 {
				return fmt.Errorf("unexpected upload artifact type, owner, mode or links")
			}
		}
		for i, st := range stats {
			other := 1 - i
			if exists[i] && st.Nlink == 2 && (!exists[other] || stats[other].Dev != st.Dev || stats[other].Ino != st.Ino || stats[other].Nlink != 2) {
				return fmt.Errorf("upload artifact has an unowned hard link")
			}
		}
		for i, name := range names {
			if exists[i] {
				if err := unix.Unlinkat(s.fd, name, 0); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
