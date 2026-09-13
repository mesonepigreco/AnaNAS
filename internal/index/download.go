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

// FinalizeDownload saves verified bases and completes the oldest received batch
// in one transaction. The caller must prove matching durable local publication
// first. Observer records and dirty generations are preserved, including records
// created after the download or not yet delivered by inotify. This cannot itself
// establish content verification or clear local edit work.
func (d *DB) FinalizeDownload(namespace string, sequence uint64, operation string, manifests []*manifest.Manifest) error {
	if !manifest.ValidID(namespace) || !manifest.ValidID(operation) || sequence == 0 || len(manifests) > journal.MaxEntries {
		return fmt.Errorf("bounded download completion required")
	}
	encoded := make([][]byte, len(manifests))
	var blocks int
	var size int64
	for i, m := range manifests {
		if m == nil || m.BlockSize != 65536 {
			return fmt.Errorf("fixed-block download manifest required")
		}
		blocks += len(m.Content.Blocks)
		if blocks > manifest.MaxBlocks+journal.MaxEntries || m.Content.Size < 0 || m.Content.Size > (8<<30)-size {
			return fmt.Errorf("download manifests exceed batch bound")
		}
		size += m.Content.Size
		var err error
		encoded[i], err = manifest.Encode(m)
		if err != nil {
			return err
		}
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		s, err := readRemoteState(tx)
		if err != nil {
			return err
		}
		if s.Namespace != namespace {
			return ErrStale
		}
		batch, err := nextRemote(tx, s)
		if err != nil {
			return err
		}
		if batch == nil || batch.Sequence != sequence || batch.ID != operation || len(batch.Entries) != len(manifests) {
			return ErrStale
		}
		for i, e := range batch.Entries {
			m, v := manifests[i].Content, e.Next
			if m.ID != v.ID || m.Size != v.Size || m.Directory != v.Directory || m.Tombstone != v.Tombstone {
				return ErrStale
			}
			if raw := tx.Bucket(files).Get([]byte(e.Path)); raw != nil {
				var r Record
				if err := json.Unmarshal(raw, &r); err != nil {
					return err
				}
				if r.Excluded {
					return ErrStale
				}
			}
			base, err := readBase(tx, e.Path)
			if err != nil {
				return err
			}
			if baseID(base) != e.Expected {
				if baseID(base) != v.ID || !bytes.Equal(tx.Bucket(bases).Get([]byte(e.Path)), encoded[i]) {
					return ErrStale
				}
			}
			if err := tx.Bucket(bases).Put([]byte(e.Path), encoded[i]); err != nil {
				return err
			}
			if err := tx.Bucket(syncIssues).Delete([]byte(e.Path)); err != nil {
				return err
			}
		}
		if err := recordActivity(tx, batch.Entries, "from NAS", time.Now()); err != nil {
			return err
		}
		return completeRemote(tx, s, batch)
	})
}
