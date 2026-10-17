package index

import (
	"encoding/json"
	bolt "go.etcd.io/bbolt"
)

// EnsurePublishedObservation closes the interval between download publication
// and inotify's first scan. A quick rename/delete must not lose the old path's
// deletion obligation simply because it was never in the observer index.
// Never replace a newer observation made concurrently by the scanner.
func (d *DB) EnsurePublishedObservation(r Record) error {
	if err := syncPath(r.Path); err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(files)
		if b.Get([]byte(r.Path)) != nil {
			return nil
		}
		r.Generation, r.Dirty = 1, true
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(r.Path), raw); err != nil {
			return err
		}
		return updateDirty(tx, r)
	})
}
