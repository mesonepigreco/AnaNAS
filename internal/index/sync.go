package index

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"

	bolt "go.etcd.io/bbolt"
)

var (
	dirty      = []byte("dirty-v1")
	bases      = []byte("bases-v1")
	pathStates = []byte("path-state-v1")
	ErrStale   = errors.New("sync state changed; compare again")
)

func initSyncState(tx *bolt.Tx) error {
	for _, name := range [][]byte{bases, pathStates, activityCounts, activityRecent} {
		if _, err := tx.CreateBucketIfNotExists(name); err != nil {
			return err
		}
	}
	if tx.Bucket(dirty) == nil {
		if _, err := tx.CreateBucket(dirty); err != nil {
			return err
		}
		// One-time migration reads only the existing local index, not the roots.
		if err := tx.Bucket(files).ForEach(func(k, v []byte) error {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			return updateDirty(tx, r)
		}); err != nil {
			return err
		}
	}
	m := tx.Bucket(meta)
	if m.Get([]byte("client-id")) == nil {
		id, err := hash.RandomDigest()
		if err != nil {
			return err
		}
		return m.Put([]byte("client-id"), []byte(id.Hex()))
	}
	return nil
}

func updateDirty(tx *bolt.Tx, r Record) error {
	if r.Dirty && !r.Excluded {
		return tx.Bucket(dirty).Put([]byte(r.Path), []byte{1})
	}
	return tx.Bucket(dirty).Delete([]byte(r.Path))
}

// DirtyPage enumerates only pending metadata records in bounded pages. The
// scheduler must separately enforce exclusions, availability, pause and LAN
// gates before reading content. No transaction survives this call.
func (d *DB) DirtyPage(after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("page size must be in [1,1000]")
	}
	out := make([]Record, 0, limit)
	err := d.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(dirty).Cursor()
		k, _ := c.First()
		if after != "" {
			k, _ = c.Seek([]byte(after))
			if string(k) == after {
				k, _ = c.Next()
			}
		}
		for ; k != nil && len(out) < limit; k, _ = c.Next() {
			var r Record
			if err := json.Unmarshal(tx.Bucket(files).Get(k), &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

func (d *DB) ClientID() (string, error) {
	var id string
	err := d.db.View(func(tx *bolt.Tx) error {
		id = string(tx.Bucket(meta).Get([]byte("client-id")))
		if !manifest.ValidID(id) {
			return fmt.Errorf("invalid persistent client ID")
		}
		return nil
	})
	return id, err
}

// Paused is a durable user intent, distinct from capability/LAN suspension.
// Resuming this flag alone can never authorize a transfer.
func (d *DB) Paused() (bool, error) {
	var paused bool
	err := d.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(meta).Get([]byte("paused"))
		if v != nil && string(v) != "0" && string(v) != "1" {
			return fmt.Errorf("invalid pause state")
		}
		paused = string(v) == "1"
		return nil
	})
	return paused, err
}

func (d *DB) SetPaused(paused bool) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		v := []byte("0")
		if paused {
			v = []byte("1")
		}
		if bytes.Equal(tx.Bucket(meta).Get([]byte("paused")), v) {
			return nil
		}
		return tx.Bucket(meta).Put([]byte("paused"), v)
	})
}

func syncPath(p string) error {
	if len(p) > 4096 || p == "." || !fs.ValidPath(p) || strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("invalid sync path")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".nas-sync" {
			return fmt.Errorf("internal metadata path is excluded")
		}
	}
	return nil
}

func readBase(tx *bolt.Tx, p string) (*manifest.Manifest, error) {
	raw := tx.Bucket(bases).Get([]byte(p))
	if raw == nil {
		return nil, nil
	}
	return manifest.Decode(raw)
}

func baseID(m *manifest.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Content.ID
}

func (d *DB) Base(p string) (*manifest.Manifest, error) {
	if err := syncPath(p); err != nil {
		return nil, err
	}
	var m *manifest.Manifest
	err := d.db.View(func(tx *bolt.Tx) error { var err error; m, err = readBase(tx, p); return err })
	return m, err
}

// Acknowledge records an already durably committed/applied version. It is not
// permission to publish content. The expected base serializes completions; an
// intervening local generation keeps dirty work queued even when its older
// version was successfully transferred. Excluded paths cannot be acknowledged.
// The bool reports whether this exact observed generation was cleared.
func (d *DB) Acknowledge(p, expectedBase string, generation uint64, next *manifest.Manifest) (bool, error) {
	if err := syncPath(p); err != nil {
		return false, err
	}
	if generation == 0 {
		return false, fmt.Errorf("observed generation is required")
	}
	encoded, err := manifest.Encode(next)
	if err != nil {
		return false, err
	}
	cleared := false
	err = d.db.Update(func(tx *bolt.Tx) error {
		old, err := readBase(tx, p)
		if err != nil {
			return err
		}
		if baseID(old) != expectedBase {
			return ErrStale
		}
		if old != nil && old.Content.ID == next.Content.ID && !bytes.Equal(tx.Bucket(bases).Get([]byte(p)), encoded) {
			return fmt.Errorf("immutable version identity reused for different content")
		}
		var r Record
		raw := tx.Bucket(files).Get([]byte(p))
		if raw == nil {
			return ErrStale
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.Excluded || r.Generation < generation {
			return ErrStale
		}
		if err := tx.Bucket(bases).Put([]byte(p), encoded); err != nil {
			return err
		}
		if r.Generation == generation {
			r.Dirty = false
			cleared = true
			v, err := json.Marshal(r)
			if err != nil {
				return err
			}
			if err := tx.Bucket(files).Put([]byte(p), v); err != nil {
				return err
			}
			return updateDirty(tx, r)
		}
		return nil
	})
	return cleared && err == nil, err
}

// Conflict records identities for an unresolved comparison. The transfer
// engine must retain the referenced immutable candidates; this record alone is
// not a backup of their bytes.
type Conflict struct {
	ID         string `json:"id"`
	BaseID     string `json:"baseId"`
	LocalID    string `json:"localId"`
	RemoteID   string `json:"remoteId"`
	Generation uint64 `json:"generation"`
}

type PathState struct {
	Paused   bool      `json:"paused"`
	Conflict *Conflict `json:"conflict,omitempty"`
}

func readPathState(tx *bolt.Tx, p string) (PathState, error) {
	var s PathState
	raw := tx.Bucket(pathStates).Get([]byte(p))
	if raw == nil {
		return s, nil
	}
	if len(raw) > 4096 {
		return s, fmt.Errorf("path state exceeds size limit")
	}
	err := json.Unmarshal(raw, &s)
	return s, err
}

func (d *DB) PathState(p string) (PathState, error) {
	if err := syncPath(p); err != nil {
		return PathState{}, err
	}
	var s PathState
	err := d.db.View(func(tx *bolt.Tx) error { var err error; s, err = readPathState(tx, p); return err })
	return s, err
}

func (d *DB) RecordConflict(p string, c Conflict) error {
	if err := syncPath(p); err != nil {
		return err
	}
	if !manifest.ValidID(c.ID) || c.Generation == 0 {
		return fmt.Errorf("invalid conflict identity or generation")
	}
	for _, id := range []string{c.BaseID, c.LocalID, c.RemoteID} {
		if id != "" && !manifest.ValidID(id) {
			return fmt.Errorf("invalid conflict version identity")
		}
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		m, err := readBase(tx, p)
		if err != nil {
			return err
		}
		if baseID(m) != c.BaseID {
			return ErrStale
		}
		var r Record
		raw := tx.Bucket(files).Get([]byte(p))
		if raw == nil {
			return ErrStale
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.Excluded || r.Generation != c.Generation {
			return ErrStale
		}
		s, err := readPathState(tx, p)
		if err != nil {
			return err
		}
		// Never replace an unresolved candidate implicitly.
		if s.Conflict != nil && *s.Conflict != c {
			return ErrStale
		}
		s.Conflict = &c
		encoded, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return tx.Bucket(pathStates).Put([]byte(p), encoded)
	})
}

// KeepDifferent persists the explicit choice to leave both versions in place.
// The conflict identities stay pinned; a stale UI choice cannot pause a newer
// conflict accidentally. Actual content is untouched.
func (d *DB) KeepDifferent(p, conflictID string) error {
	if err := syncPath(p); err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		s, err := readPathState(tx, p)
		if err != nil {
			return err
		}
		if s.Conflict == nil || s.Conflict.ID != conflictID {
			return ErrStale
		}
		s.Paused = true
		encoded, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return tx.Bucket(pathStates).Put([]byte(p), encoded)
	})
}
