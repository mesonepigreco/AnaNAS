// Package index persists observed metadata and dirty generations without content
// reads. All updates are bounded batches; bbolt owns the on-disk tree.
package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var files = []byte("files")
var meta = []byte("meta")

type Fingerprint struct {
	Device, Inode          uint64
	Size, MtimeNS, CtimeNS int64
	Mode                   uint32
}
type Record struct {
	Path        string
	Fingerprint Fingerprint
	Generation  uint64
	Dirty       bool
	Missing     bool
	Excluded    bool
	Scan        uint64
}

type DB struct {
	db       *bolt.DB
	uploadMu sync.Mutex
}

func Open(filename, root string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filename, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	d := &DB{db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		if _, e := tx.CreateBucketIfNotExists(files); e != nil {
			return e
		}
		m, e := tx.CreateBucketIfNotExists(meta)
		if e != nil {
			return e
		}
		if old := m.Get([]byte("root")); old != nil && string(old) != root {
			return fmt.Errorf("index belongs to another root")
		}
		if e := initSyncState(tx); e != nil {
			return e
		}
		return m.Put([]byte("root"), []byte(root))
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}
func (d *DB) Close() error { return d.db.Close() }

// Put increments generations when metadata changes or an explicit write event
// invalidates the cache. Seen-only updates never erase pending dirty work.
func (d *DB) Put(records []Record, force bool) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(files)
		for _, r := range records {
			var old Record
			raw := b.Get([]byte(r.Path))
			if raw != nil {
				if err := json.Unmarshal(raw, &old); err != nil {
					return err
				}
			}
			changed := raw == nil || old.Fingerprint != r.Fingerprint || old.Missing != r.Missing || old.Excluded != r.Excluded || force
			r.Generation = old.Generation
			r.Dirty = old.Dirty
			if changed {
				r.Generation++
				r.Dirty = true
			}
			if r.Scan == 0 {
				r.Scan = old.Scan
			}
			encoded, err := json.Marshal(r)
			if err != nil {
				return err
			}
			if err = b.Put([]byte(r.Path), encoded); err != nil {
				return err
			}
			if err = updateDirty(tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// Page returns a bounded ordered page after a cursor, without retaining an mmap
// transaction while callers perform filesystem I/O.
func (d *DB) Page(after string, limit int) ([]Record, error) {
	return d.PagePrefix("", after, limit)
}

func (d *DB) PagePrefix(prefix, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("page size must be in [1,1000]")
	}
	out := make([]Record, 0, limit)
	err := d.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(files).Cursor()
		k, v := c.Seek([]byte(prefix))
		if after >= prefix && after != "" {
			k, v = c.Seek([]byte(after))
			if string(k) == after {
				k, v = c.Next()
			}
		}
		for ; k != nil && strings.HasPrefix(string(k), prefix) && len(out) < limit; k, v = c.Next() {
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}
func (d *DB) Get(p string) (Record, bool, error) {
	var r Record
	var ok bool
	err := d.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(files).Get([]byte(p))
		if raw == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(raw, &r)
	})
	return r, ok, err
}

func (d *DB) SetRecovery(required bool) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		v := []byte("0")
		if required {
			v = []byte("1")
		}
		return tx.Bucket(meta).Put([]byte("recovery"), v)
	})
}
func (d *DB) RecoveryRequired() (bool, error) {
	var required bool
	err := d.db.View(func(tx *bolt.Tx) error { required = string(tx.Bucket(meta).Get([]byte("recovery"))) != "0"; return nil })
	return required, err
}

// NextScan assigns a persisted logical scan ID, independent of wall clocks.
func (d *DB) NextScan() (uint64, error) {
	var id uint64
	err := d.db.Update(func(tx *bolt.Tx) error { var err error; id, err = tx.Bucket(meta).NextSequence(); return err })
	return id, err
}
