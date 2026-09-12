package index

import (
	"context"
	"encoding/json"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/manifest"
	"os"
	"sort"
	"strings"
)

type DirectorySize struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	LocalBytes  int64  `json:"localBytes"`
	SyncedBytes int64  `json:"syncedBytes"`
	Files       int    `json:"files"`
}
type Usage struct {
	DirectorySize
	Children  []DirectorySize `json:"children"`
	Truncated bool            `json:"truncated"`
}

// DirectoryUsage aggregates indexed logical sizes, never walks either filesystem.
// SyncedBytes is the last confirmed shared version, not a fresh NAS enumeration.
func (d *DB) DirectoryUsage(ctx context.Context, path string) (Usage, error) {
	result := Usage{DirectorySize: DirectorySize{Path: path, Name: "NASdir"}, Children: []DirectorySize{}}
	if path != "" {
		if err := syncPath(path); err != nil {
			return result, err
		}
		result.Name = path[strings.LastIndex(path, "/")+1:]
	}
	prefix := path
	if prefix != "" {
		prefix += "/"
	}
	children := map[string]*DirectorySize{}
	add := func(p string, size int64, directory, synced bool) error {
		rel := strings.TrimPrefix(p, prefix)
		if rel == "" {
			return nil
		}
		name, _, nested := strings.Cut(rel, "/")
		if size < 0 {
			return fmt.Errorf("negative indexed size")
		}
		if synced {
			result.SyncedBytes += size
		} else {
			result.LocalBytes += size
			if !directory {
				result.Files++
			}
		}
		if !nested && !directory {
			return nil
		}
		child := children[name]
		if child == nil {
			child = &DirectorySize{Path: prefix + name, Name: name}
			children[name] = child
		}
		if synced {
			child.SyncedBytes += size
		} else {
			child.LocalBytes += size
			if !directory {
				child.Files++
			}
		}
		return nil
	}
	err := d.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(files).Cursor()
		for k, v := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var r Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.Missing || r.Excluded {
				continue
			}
			kind := os.FileMode(r.Fingerprint.Mode)
			if !kind.IsRegular() && !kind.IsDir() {
				continue
			}
			size := r.Fingerprint.Size
			if kind.IsDir() {
				size = 0
			}
			if err := add(string(k), size, kind.IsDir(), false); err != nil {
				return err
			}
		}
		c = tx.Bucket(bases).Cursor()
		for k, v := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			m, err := manifest.Decode(v)
			if err != nil {
				return err
			}
			if m.Content.Tombstone {
				continue
			}
			if m.Content.Directory {
				state, err := readPathState(tx, string(k))
				if err != nil {
					return err
				}
				// Retired archive paths have no visible directory. Their retained zero-
				// byte history must not recreate a ghost row in the storage browser.
				if state.Paused {
					var r Record
					if raw := tx.Bucket(files).Get(k); raw != nil {
						if err := json.Unmarshal(raw, &r); err != nil {
							return err
						}
					}
					if r.Missing || r.Path == "" {
						continue
					}
				}
			}
			size := m.Content.Size
			if m.Content.Directory {
				size = 0
			}
			if err := add(string(k), size, m.Content.Directory, true); err != nil {
				return err
			}
		}
		return nil
	})
	for _, child := range children {
		result.Children = append(result.Children, *child)
	}
	sort.Slice(result.Children, func(i, j int) bool {
		a, b := result.Children[i], result.Children[j]
		if a.LocalBytes == b.LocalBytes {
			return a.Name < b.Name
		}
		return a.LocalBytes > b.LocalBytes
	})
	if len(result.Children) > 1000 {
		result.Children = result.Children[:1000]
		result.Truncated = true
	}
	return result, err
}
