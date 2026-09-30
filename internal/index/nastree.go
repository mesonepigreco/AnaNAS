package index

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/journal"
)

var nasHeads = []byte("nas-inventory-heads-v1")
var nasInventoryKey = []byte("nas-inventory-cursor-v1")

type NASCursor struct {
	Namespace string    `json:"namespace"`
	Policy    string    `json:"policy"`
	Through   uint64    `json:"through"`
	CheckedAt time.Time `json:"checkedAt"`
	Complete  bool      `json:"complete"`
}

func readNASCursor(tx *bolt.Tx) (s NASCursor, err error) {
	if raw := tx.Bucket(meta).Get(nasInventoryKey); raw != nil {
		err = json.Unmarshal(raw, &s)
	}
	return
}
func (d *DB) NASCursor() (s NASCursor, err error) {
	err = d.db.View(func(tx *bolt.Tx) error { s, err = readNASCursor(tx); return err })
	return
}

// RecordNASPage indexes the NAS's authenticated versioned inventory separately
// from download receipts. Merely listing online files never acknowledges them.
func (d *DB) RecordNASPage(page changefeed.Page) error {
	if err := page.Validate(page.After, journal.MaxPage); err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		s, err := readNASCursor(tx)
		if err != nil {
			return err
		}
		if s.Through != page.After || (s.Namespace != "" && (s.Namespace != page.Namespace || s.Policy != page.Policy)) {
			return ErrStale
		}
		if err := bindReplicaNamespace(tx, page.Namespace); err != nil {
			return err
		}
		b, err := tx.CreateBucketIfNotExists(nasHeads)
		if err != nil {
			return err
		}
		for _, batch := range page.Batches {
			for _, entry := range batch.Entries {
				if entry.Next.Tombstone {
					if err := b.Delete([]byte(entry.Path)); err != nil {
						return err
					}
					continue
				}
				raw, err := json.Marshal(entry.Next)
				if err != nil {
					return err
				}
				if err := b.Put([]byte(entry.Path), raw); err != nil {
					return err
				}
			}
		}
		s = NASCursor{page.Namespace, page.Policy, page.Through, time.Now(), len(page.Batches) < journal.MaxPage}
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return tx.Bucket(meta).Put(nasInventoryKey, raw)
	})
}

type NASNode struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Directory   bool   `json:"directory"`
	Files       int    `json:"files"`
	SyncedFiles int    `json:"syncedFiles"`
	Bytes       int64  `json:"bytes"`
}
type NASTree struct {
	NASNode
	Children  []NASNode `json:"children"`
	Next      string    `json:"next"`
	Inventory NASCursor `json:"inventory"`
	Priority  string    `json:"priority"`
}

func (d *DB) NASTree(ctx context.Context, directory, after string) (result NASTree, err error) {
	if directory != "" {
		if err = syncPath(directory); err != nil {
			return
		}
	}
	if len(after) > 4096 {
		return result, fmt.Errorf("invalid directory cursor")
	}
	result.NASNode = NASNode{Path: directory, Name: path.Base(directory), Directory: true}
	if directory == "" {
		result.Name = "NAS"
	}
	result.Children = []NASNode{}
	prefix := directory
	if prefix != "" {
		prefix += "/"
	}
	children := map[string]*NASNode{}
	err = d.db.View(func(tx *bolt.Tx) error {
		var err error
		result.Inventory, err = readNASCursor(tx)
		if err != nil {
			return err
		}
		result.Priority = string(tx.Bucket(meta).Get(remotePriorityKey))
		b := tx.Bucket(nasHeads)
		if b == nil {
			return nil
		}
		cursor := b.Cursor()
		for k, v := cursor.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = cursor.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			p := string(k)
			if p == directory || internalTrialPath(p) {
				continue
			}
			var version journal.Version
			if err := json.Unmarshal(v, &version); err != nil {
				return err
			}
			name, _, nested := strings.Cut(strings.TrimPrefix(p, prefix), "/")
			child := children[name]
			if child == nil {
				child = &NASNode{Path: prefix + name, Name: name, Directory: nested || version.Directory}
				children[name] = child
			}
			child.Directory = child.Directory || nested || version.Directory
			if version.Directory {
				continue
			}
			result.Files++
			result.Bytes += version.Size
			child.Files++
			child.Bytes += version.Size
			// Receipt of metadata is not proof of a local copy. Require the exact NAS
			// version and a clean, present observed local file (same-size edits count).
			base, err := readBase(tx, p)
			if err != nil {
				return err
			}
			var local Record
			if raw := tx.Bucket(files).Get(k); raw != nil {
				if err := json.Unmarshal(raw, &local); err != nil {
					return err
				}
			}
			if base != nil && base.Content.ID == version.ID && !local.Missing && !local.Excluded && !local.Dirty && local.Path != "" {
				result.SyncedFiles++
				child.SyncedFiles++
			}
		}
		return nil
	})
	names := make([]string, 0, len(children))
	for name := range children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name <= after {
			continue
		}
		if len(result.Children) == 200 {
			result.Next = result.Children[len(result.Children)-1].Name
			break
		}
		result.Children = append(result.Children, *children[name])
	}
	return
}
