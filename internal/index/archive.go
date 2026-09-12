package index

import (
	"encoding/json"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
)

// RetireArchivedDirectories pauses only the supplied obsolete directory paths.
// The maintenance caller must verify archival copies before removing the visible
// originals. Bases/history stay intact; recreating a retired path needs an
// explicit later decision. Files are not paused, so their deletions can sync.
func (d *DB) RetireArchivedDirectories(paths []string) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		for _, p := range paths {
			if err := syncPath(p); err != nil {
				return err
			}
			var r Record
			if err := json.Unmarshal(tx.Bucket(files).Get([]byte(p)), &r); err != nil {
				return err
			}
			if !os.FileMode(r.Fingerprint.Mode).IsDir() {
				return fmt.Errorf("archive retirement requires a directory: %s", p)
			}
			state, err := readPathState(tx, p)
			if err != nil {
				return err
			}
			if state.Conflict != nil {
				return fmt.Errorf("cannot retire a conflicted directory: %s", p)
			}
			state.Paused = true
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err = tx.Bucket(pathStates).Put([]byte(p), raw); err != nil {
				return err
			}
		}
		return nil
	})
}

// RetireArchivedTombstones finishes an explicit archive after file deletion is
// already acknowledged. It cannot pause a live file or an unresolved conflict.
func (d *DB) RetireArchivedTombstones(paths []string) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		for _, p := range paths {
			if err := syncPath(p); err != nil {
				return err
			}
			base, err := readBase(tx, p)
			if err != nil {
				return err
			}
			if base == nil || !base.Content.Tombstone {
				return fmt.Errorf("archived file deletion is not acknowledged: %s", p)
			}
			var r Record
			if err = json.Unmarshal(tx.Bucket(files).Get([]byte(p)), &r); err != nil {
				return err
			}
			if !r.Missing {
				return fmt.Errorf("archived path still exists: %s", p)
			}
			state, err := readPathState(tx, p)
			if err != nil {
				return err
			}
			if state.Conflict != nil {
				return fmt.Errorf("archived path has a conflict: %s", p)
			}
			state.Paused = true
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err = tx.Bucket(pathStates).Put([]byte(p), raw); err != nil {
				return err
			}
		}
		return nil
	})
}
