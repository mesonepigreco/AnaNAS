package index

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	bolt "go.etcd.io/bbolt"
)

var syncRequests = []byte("sync-requests-v1")

var legacyTrialRoot = regexp.MustCompile(`^anaNAS-(deletions|folders)-[a-f0-9]{12}$`)

// Internal trial records remain in the index for recovery, but are not user work.
func internalTrialPath(path string) bool {
	root, _, _ := strings.Cut(path, "/")
	return root == ".ananas-tests" || path == "anaNAS-live-check.bin" || legacyTrialRoot.MatchString(root)
}

type PendingFile struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Generation  uint64 `json:"generation"`
	Action      string `json:"action"`
	Reason      string `json:"reason"`
	Confirmable bool   `json:"confirmable"`
	Requested   bool   `json:"requested"`
}

type PendingFiles struct {
	Items []PendingFile `json:"items"`
	Next  string        `json:"next"`
	Total int           `json:"total"`
}

type SyncConfirmation struct {
	Path       string `json:"path"`
	All        bool   `json:"all"`
	Generation uint64 `json:"generation"`
}

type ConfirmationResult struct {
	Queued  int `json:"queued"`
	Blocked int `json:"blocked"`
}

func pendingFile(tx *bolt.Tx, r Record, limit int64) (PendingFile, error) {
	p := PendingFile{Path: r.Path, Size: r.Fingerprint.Size, Generation: r.Generation, Action: "Upload", Reason: "Waiting to sync", Confirmable: true, Requested: tx.Bucket(syncRequests).Get([]byte(r.Path)) != nil}
	mode := os.FileMode(r.Fingerprint.Mode)
	if mode.IsDir() {
		p.Action, p.Size = "Create folder", 0
	}
	if r.Missing {
		p.Action, p.Size = "Delete from NAS", 0
	}
	if reason := UnsupportedReason(r, limit); reason != "" {
		p.Reason, p.Confirmable = reason, false
		return p, nil
	}
	if r.Missing {
		base, err := readBase(tx, r.Path)
		if err != nil {
			return p, err
		}
		if base != nil && base.Content.Directory {
			prefix := r.Path + "/"
			k, _ := tx.Bucket(files).Cursor().Seek([]byte(prefix))
			if k != nil && strings.HasPrefix(string(k), prefix) {
				p.Reason, p.Confirmable = "Folder deletion needs child reconciliation", false
				return p, nil
			}
		}
	}
	s, err := readPathState(tx, r.Path)
	if err != nil {
		return p, err
	}
	switch {
	case s.Conflict != nil:
		p.Reason, p.Confirmable = "Conflict needs a version choice", false
	case s.Paused:
		p.Reason, p.Confirmable = "Sync suspended for this path", false
	case !r.Missing && !mode.IsDir() && !mode.IsRegular():
		p.Reason, p.Confirmable = "Unsupported file type", false
	case !r.Missing && mode.IsRegular() && r.Fingerprint.Size > limit:
		p.Reason, p.Confirmable = "Exceeds the file size limit", false
	case p.Requested:
		p.Reason = "Confirmed · queued for sync"
	}
	if p.Confirmable {
		issue, err := readSyncIssue(tx, r)
		if err != nil {
			return p, err
		}
		if issue != nil {
			p.Reason = issue.Reason
		}
	}
	return p, nil
}

// PendingSync uses only the durable dirty index. Pagination never enumerates
// filesystem contents or mistakes confirmation for a completed transfer.
func (d *DB) PendingSync(ctx context.Context, after string, limit int64) (PendingFiles, error) {
	result := PendingFiles{Items: []PendingFile{}}
	// The cursor is an observed index key, not a path to publish. Unsupported
	// names must remain usable as page boundaries so later files stay visible.
	if len(after) > 4096 {
		return result, fmt.Errorf("pending cursor too long")
	}
	err := d.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(dirty).Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if internalTrialPath(string(k)) {
				continue
			}
			result.Total++
			if string(k) <= after {
				continue
			}
			if len(result.Items) == 200 {
				result.Next = result.Items[199].Path
				continue
			}
			var r Record
			if err := json.Unmarshal(tx.Bucket(files).Get(k), &r); err != nil {
				return err
			}
			p, err := pendingFile(tx, r, limit)
			if err != nil {
				return err
			}
			result.Items = append(result.Items, p)
		}
		return nil
	})
	return result, err
}

// ConfirmSync durably prioritizes eligible work. It never unpauses, resolves a
// conflict, clears dirty generations, or acknowledges a NAS version.
func (d *DB) ConfirmSync(ctx context.Context, request SyncConfirmation, limit int64) (ConfirmationResult, error) {
	var result ConfirmationResult
	if request.All {
		if request.Path != "" || request.Generation != 0 {
			return result, fmt.Errorf("all cannot include a path or generation")
		}
	} else if err := syncPath(request.Path); err != nil || request.Generation == 0 {
		return result, fmt.Errorf("path and current generation required")
	}
	err := d.db.Update(func(tx *bolt.Tx) error {
		queue := func(k []byte, generation uint64) error {
			if internalTrialPath(string(k)) {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			var r Record
			if raw := tx.Bucket(files).Get(k); raw != nil {
				if err := json.Unmarshal(raw, &r); err != nil {
					return err
				}
			}
			if generation != 0 && r.Generation != generation {
				return ErrStale
			}
			if !r.Dirty || r.Excluded {
				return nil
			}
			p, err := pendingFile(tx, r, limit)
			if err != nil {
				return err
			}
			if !p.Confirmable {
				result.Blocked++
				return nil
			}
			result.Queued++
			if err := tx.Bucket(syncIssues).Delete(k); err != nil {
				return err
			}
			return tx.Bucket(syncRequests).Put(k, []byte{1})
		}
		if request.All {
			return tx.Bucket(dirty).ForEach(func(k, _ []byte) error { return queue(k, 0) })
		}
		if err := queue([]byte(request.Path), request.Generation); err != nil {
			return err
		}
		// A newly created file needs its pending parent directories published first.
		if result.Queued > 0 {
			for p := request.Path; strings.Contains(p, "/"); {
				p = p[:strings.LastIndex(p, "/")]
				if err := queue([]byte(p), 0); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return ConfirmationResult{}, err
	}
	return result, nil
}

func (d *DB) RequestedPage(after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid page limit")
	}
	result := []Record{}
	err := d.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(syncRequests).Cursor()
		for k, _ := c.Seek([]byte(after)); k != nil && len(result) < limit; k, _ = c.Next() {
			if string(k) == after {
				continue
			}
			var r Record
			if err := json.Unmarshal(tx.Bucket(files).Get(k), &r); err != nil {
				return err
			}
			result = append(result, r)
		}
		return nil
	})
	return result, err
}
