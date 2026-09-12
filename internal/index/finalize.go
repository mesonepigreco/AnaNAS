package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

// FinalizeUpload atomically records verified immutable manifests and releases
// the matching committed outbox. Its caller must first establish those versions
// in the local replica journal. It does no content I/O and cannot authenticate
// block hashes itself. Later observed generations remain dirty.
func (d *DB) FinalizeUpload(namespace, operation string, manifests []*manifest.Manifest) error {
	if !manifest.ValidID(namespace) || !manifest.ValidID(operation) || len(manifests) < 1 || len(manifests) > journal.MaxEntries {
		return fmt.Errorf("bounded upload completion required")
	}
	encoded := make([][]byte, len(manifests))
	var blocks int
	for i, m := range manifests {
		if m == nil || m.BlockSize != 65536 {
			return fmt.Errorf("fixed-block upload manifest required")
		}
		blocks += len(m.Content.Blocks)
		if blocks > manifest.MaxBlocks+journal.MaxEntries {
			return fmt.Errorf("aggregate upload manifests exceed bound")
		}
		var err error
		encoded[i], err = manifest.Encode(m)
		if err != nil {
			return err
		}
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		if string(tx.Bucket(meta).Get(replicaNamespaceKey)) != namespace {
			return ErrStale
		}
		u, err := readUpload(tx)
		if err != nil {
			return err
		}
		if u == nil {
			if string(tx.Bucket(meta).Get(lastUploadKey)) == operation {
				return nil
			}
			return ErrStale
		}
		if u.Namespace != namespace || u.Proposal.ID != operation || u.Receipt == nil || len(u.Proposal.Entries) != len(manifests) {
			return ErrStale
		}
		for i, e := range u.Proposal.Entries {
			v, m := e.Next, manifests[i].Content
			if m.ID != v.ID || m.Size != v.Size || m.Tombstone != v.Tombstone || m.Directory != v.Directory {
				return ErrStale
			}
			base, err := readBase(tx, e.Path)
			if err != nil {
				return err
			}
			if baseID(base) != e.Expected {
				// Permit an identical earlier per-file acknowledgement, but
				// never overwrite another base or a reused version's hashes.
				if baseID(base) != v.ID || !bytes.Equal(tx.Bucket(bases).Get([]byte(e.Path)), encoded[i]) {
					return ErrStale
				}
			}
			var r Record
			if err := json.Unmarshal(tx.Bucket(files).Get([]byte(e.Path)), &r); err != nil {
				return err
			}
			if r.Excluded || r.Generation < u.Generations[i] {
				return ErrStale
			}
			if err := tx.Bucket(bases).Put([]byte(e.Path), encoded[i]); err != nil {
				return err
			}
			if r.Generation == u.Generations[i] {
				r.Dirty = false
				raw, err := json.Marshal(r)
				if err != nil {
					return err
				}
				if err := tx.Bucket(files).Put([]byte(e.Path), raw); err != nil {
					return err
				}
				if err := updateDirty(tx, r); err != nil {
					return err
				}
			}
		}
		if err := tx.Bucket(meta).Put(lastUploadKey, []byte(operation)); err != nil {
			return err
		}
		if err := recordActivity(tx, u.Proposal.Entries, "to NAS", time.Now()); err != nil {
			return err
		}
		return tx.Bucket(meta).Delete(uploadKey)
	})
}
