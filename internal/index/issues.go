package index

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

var syncIssues = []byte("sync-issues-v1")

// SyncIssue concerns one observed generation, never a completed transfer.
type SyncIssue struct {
	Generation uint64 `json:"generation"`
	Reason     string `json:"reason"`
	Retry      bool   `json:"retry"`
}

// UnsupportedReason is shared by scheduling and the pending-file UI. Linux
// names that the protocol cannot represent remain visible but cannot poison a batch.
func UnsupportedReason(r Record, limit int64) string {
	if err := syncPath(r.Path); err != nil {
		return "Unsupported sync path (including backslash names)"
	}
	mode := os.FileMode(r.Fingerprint.Mode)
	if !r.Missing && !mode.IsRegular() && !mode.IsDir() {
		return "Unsupported file type"
	}
	if !r.Missing && mode.IsRegular() {
		if r.Fingerprint.Size < 0 {
			return "Invalid indexed file size"
		}
		if r.Fingerprint.Size > limit {
			return "Exceeds the file size limit"
		}
	}
	return ""
}

func readSyncIssue(tx *bolt.Tx, r Record) (*SyncIssue, error) {
	raw := tx.Bucket(syncIssues).Get([]byte(r.Path))
	if raw == nil {
		return nil, nil
	}
	var issue SyncIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return nil, err
	}
	if issue.Generation != r.Generation {
		return nil, nil
	}
	return &issue, nil
}

func (d *DB) SyncIssue(r Record) (*SyncIssue, error) {
	var issue *SyncIssue
	err := d.db.View(func(tx *bolt.Tx) error { var err error; issue, err = readSyncIssue(tx, r); return err })
	return issue, err
}

func (d *DB) RecordSyncIssue(path string, generation uint64, reason string, retry bool) error {
	if generation == 0 || reason == "" || len(path) > 4096 {
		return fmt.Errorf("observed path and failure required")
	}
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		var r Record
		if raw := tx.Bucket(files).Get([]byte(path)); raw != nil {
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
		}
		if !r.Dirty || r.Excluded || r.Generation != generation {
			return nil
		}
		issue := SyncIssue{generation, reason, retry}
		old, err := readSyncIssue(tx, r)
		if err != nil {
			return err
		}
		if old != nil && *old == issue {
			return nil
		}
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		return tx.Bucket(syncIssues).Put([]byte(path), raw)
	})
}

// A retry begins a new bounded pass. Permanent issues wait for an observed
// generation change, remote base update, or explicit user confirmation.
func (d *DB) ClearTransientIssues(ctx context.Context) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		var keys [][]byte
		if err := tx.Bucket(syncIssues).ForEach(func(k, v []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var issue SyncIssue
			if err := json.Unmarshal(v, &issue); err != nil {
				return err
			}
			if issue.Retry {
				keys = append(keys, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range keys {
			if err := tx.Bucket(syncIssues).Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}
