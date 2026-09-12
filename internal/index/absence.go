package index

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// AcknowledgeAbsent clears only an exact observed missing generation that has
// no acknowledged base. The comparison caller must verify an accessible parent,
// exact local absence and authenticated remote absence. No file or base is
// deleted and no tombstone is created; later remote creation can still replay.
func (d *DB) AcknowledgeAbsent(path string, generation uint64) (bool, error) {
	if err := syncPath(path); err != nil {
		return false, err
	}
	if generation == 0 {
		return false, fmt.Errorf("observed generation required")
	}
	cleared := false
	err := d.db.Update(func(tx *bolt.Tx) error {
		base, err := readBase(tx, path)
		if err != nil {
			return err
		}
		if base != nil {
			return ErrStale
		}
		var r Record
		if err := json.Unmarshal(tx.Bucket(files).Get([]byte(path)), &r); err != nil {
			return err
		}
		if r.Excluded || !r.Missing || r.Generation != generation {
			return ErrStale
		}
		r.Dirty = false
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := tx.Bucket(files).Put([]byte(path), raw); err != nil {
			return err
		}
		if err := updateDirty(tx, r); err != nil {
			return err
		}
		cleared = true
		return nil
	})
	return cleared && err == nil, err
}
