package index

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// ClearObserved acknowledges an exact native observation after its immutable
// journal capture or comparison. It does not install a PC replica base. Any
// newer generation remains dirty, including an explicit same-metadata event.
func (d *DB) ClearObserved(want Record) (bool, error) {
	if err := syncPath(want.Path); err != nil {
		return false, err
	}
	if want.Generation == 0 || want.Excluded {
		return false, fmt.Errorf("eligible observation required")
	}
	cleared := false
	err := d.db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket(files).Get([]byte(want.Path))
		if raw == nil {
			return nil
		}
		var got Record
		if err := json.Unmarshal(raw, &got); err != nil {
			return err
		}
		if got.Generation != want.Generation || got.Fingerprint != want.Fingerprint || got.Missing != want.Missing || got.Excluded {
			return nil
		}
		cleared = true
		if !got.Dirty {
			return nil
		}
		got.Dirty = false
		encoded, err := json.Marshal(got)
		if err != nil {
			return err
		}
		if err := tx.Bucket(files).Put([]byte(got.Path), encoded); err != nil {
			return err
		}
		return updateDirty(tx, got)
	})
	return cleared && err == nil, err
}
