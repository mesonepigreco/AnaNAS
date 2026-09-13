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

// ClearUnsupportedAbsence retires an observed deletion of a name the protocol
// cannot represent. Such a name could never have been uploaded. Unlike ordinary
// tombstones, it needs no remote deletion; retained sync history prevents cleanup.
func (d *DB) ClearUnsupportedAbsence(path string, generation uint64) (bool, error) {
	if syncPath(path) == nil || generation == 0 {
		return false, nil
	}
	cleared := false
	err := d.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bases).Get([]byte(path)) != nil || tx.Bucket(pathStates).Get([]byte(path)) != nil {
			return nil
		}
		raw := tx.Bucket(files).Get([]byte(path))
		if raw == nil {
			return nil
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if !r.Missing || !r.Dirty || r.Generation != generation {
			return nil
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
