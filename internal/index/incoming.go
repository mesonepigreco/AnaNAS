package index

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

var incomingIssues = []byte("incoming-issues-v1")

// IncomingIssue preserves a publication that could not be applied visibly.
// Its remote version is retained in the replica journal/cache before acknowledgement.
type IncomingIssue struct {
	Path     string          `json:"path"`
	Revision string          `json:"revision"`
	Expected string          `json:"expected"`
	Remote   journal.Version `json:"remote"`
	Reason   string          `json:"reason"`
	Choice   string          `json:"choice,omitempty"`
	Request  string          `json:"request,omitempty"`
}
type IncomingChoice struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Choice   string `json:"choice"`
}
type IncomingPage struct {
	Signature string          `json:"signature"`
	Items     []IncomingIssue `json:"items"`
	Total     int             `json:"total"`
	Attention int             `json:"attention"`
	Next      string          `json:"next"`
}

func readIncoming(b *bolt.Bucket, path string) (*IncomingIssue, error) {
	if b == nil || b.Get([]byte(path)) == nil {
		return nil, nil
	}
	var issue IncomingIssue
	err := json.Unmarshal(b.Get([]byte(path)), &issue)
	return &issue, err
}
func (d *DB) Incoming(path string) (*IncomingIssue, error) {
	var issue *IncomingIssue
	err := d.db.View(func(tx *bolt.Tx) error {
		var err error
		issue, err = readIncoming(tx.Bucket(incomingIssues), path)
		return err
	})
	return issue, err
}
func incomingBlocked(tx *bolt.Tx, path string) (bool, error) {
	b := tx.Bucket(incomingIssues)
	if b == nil {
		return false, nil
	}
	// Exact path/ancestors, plus a bounded seek for any descendant.
	for p := path; p != ""; {
		if b.Get([]byte(p)) != nil {
			return true, nil
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			break
		}
		p = p[:i]
	}
	k, _ := b.Cursor().Seek([]byte(path + "/"))
	return k != nil && strings.HasPrefix(string(k), path+"/"), nil
}
func (d *DB) IncomingBlocked(path string) (bool, error) {
	var blocked bool
	err := d.db.View(func(tx *bolt.Tx) error { var err error; blocked, err = incomingBlocked(tx, path); return err })
	return blocked, err
}
func (d *DB) RecordIncoming(e journal.Entry, reason string) error {
	if err := syncPath(e.Path); err != nil {
		return err
	}
	if err := journal.ValidateVersion(e.Next); err != nil {
		return err
	}
	if len(reason) > 8192 {
		reason = reason[:8192]
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(incomingIssues)
		if err != nil {
			return err
		}
		prior, err := readIncoming(b, e.Path)
		if err != nil {
			return err
		}
		issue := IncomingIssue{Path: e.Path, Revision: e.Next.ID, Expected: e.Expected, Remote: e.Next, Reason: reason}
		if prior != nil {
			issue.Expected = prior.Expected
			if prior.Revision == issue.Revision || prior.Choice == "separate" {
				issue.Choice, issue.Request = prior.Choice, prior.Request
			}
		}
		if prior == nil {
			for path := e.Path; strings.Contains(path, "/"); {
				path = path[:strings.LastIndexByte(path, '/')]
				ancestor, err := readIncoming(b, path)
				if err != nil {
					return err
				}
				if ancestor != nil && ancestor.Choice == "separate" {
					issue.Choice = "separate"
					break
				}
			}
		}
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		return b.Put([]byte(e.Path), raw)
	})
}
func (d *DB) IncomingProblems(ctx context.Context, after string) (IncomingPage, error) {
	result := IncomingPage{Items: []IncomingIssue{}}
	fingerprint := sha256.New()
	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(incomingIssues)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var issue IncomingIssue
			if err := json.Unmarshal(v, &issue); err != nil {
				return err
			}
			result.Total++
			if issue.Choice != "separate" {
				result.Attention++
				fingerprint.Write(k)
				fingerprint.Write([]byte{0})
			}
			if string(k) <= after {
				return nil
			}
			if len(result.Items) == 200 {
				result.Next = result.Items[199].Path
				return nil
			}
			result.Items = append(result.Items, issue)
			return nil
		})
	})
	result.Signature = fmt.Sprintf("%x", fingerprint.Sum(nil))
	return result, err
}
func (d *DB) ChooseIncoming(request IncomingChoice, id string) error {
	if err := syncPath(request.Path); err != nil {
		return err
	}
	if !manifest.ValidID(id) || !manifest.ValidID(request.Revision) {
		return fmt.Errorf("valid recovery identity required")
	}
	if request.Choice != "retry" && request.Choice != "nas" && request.Choice != "separate" {
		return fmt.Errorf("unknown recovery choice")
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(incomingIssues)
		issue, err := readIncoming(b, request.Path)
		if err != nil {
			return err
		}
		if issue == nil || issue.Revision != request.Revision || issue.Request != "" {
			return ErrStale
		}
		if err := b.ForEach(func(k, v []byte) error {
			if !related(string(k), request.Path) {
				return nil
			}
			var other IncomingIssue
			if err := json.Unmarshal(v, &other); err != nil {
				return err
			}
			if other.Request != "" {
				return ErrStale
			}
			return nil
		}); err != nil {
			return err
		}
		issue.Choice = request.Choice
		if request.Choice != "separate" {
			issue.Request = id
		} else {
			// The choice covers existing descendants as well as future updates.
			c := b.Cursor()
			prefix := issue.Path + "/"
			for k, v := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = c.Next() {
				var child IncomingIssue
				if err := json.Unmarshal(v, &child); err != nil {
					return err
				}
				child.Choice = "separate"
				raw, err := json.Marshal(child)
				if err != nil {
					return err
				}
				if err := b.Put(k, raw); err != nil {
					return err
				}
			}
		}
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		return b.Put([]byte(issue.Path), raw)
	})
}
func (d *DB) NextIncomingRequest() (*IncomingIssue, error) {
	var result *IncomingIssue
	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(incomingIssues)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var issue IncomingIssue
			if err := json.Unmarshal(v, &issue); err != nil {
				return err
			}
			if issue.Request != "" {
				result = &issue
				break
			}
		}
		return nil
	})
	return result, err
}
func (d *DB) FinishIncomingRequest(issue IncomingIssue, failure string, subtree bool) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(incomingIssues)
		current, err := readIncoming(b, issue.Path)
		if err != nil {
			return err
		}
		if current == nil || current.Request != issue.Request || current.Revision != issue.Revision {
			return ErrStale
		}
		if failure != "" {
			current.Reason = failure
			current.Choice = ""
			current.Request = ""
			raw, err := json.Marshal(current)
			if err != nil {
				return err
			}
			return b.Put([]byte(issue.Path), raw)
		}
		if err := b.Delete([]byte(issue.Path)); err != nil {
			return err
		}
		if subtree {
			c := b.Cursor()
			prefix := issue.Path + "/"
			for k, _ := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, _ = c.Seek([]byte(prefix)) {
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// BaseEntries enumerates retained remote bases, including paths never visible
// locally. Recovery uses bounded pages, with no scan of the user's tree.
func (d *DB) BaseEntries(prefix, after string) ([]journal.Entry, error) {
	result := []journal.Entry{}
	err := d.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bases).Cursor()
		start := prefix + "/"
		if after == "" {
			m, err := readBase(tx, prefix)
			if err != nil {
				return err
			}
			if m != nil {
				result = append(result, journal.Entry{Path: prefix, Next: journal.Version{ID: m.Content.ID, Size: m.Content.Size, Directory: m.Content.Directory, Tombstone: m.Content.Tombstone}})
			}
		}
		if after > start {
			start = after
		}
		for k, _ := c.Seek([]byte(start)); k != nil && len(result) < 128; k, _ = c.Next() {
			path := string(k)
			if path <= after {
				continue
			}
			if path != prefix && !strings.HasPrefix(path, prefix+"/") {
				break
			}
			m, err := readBase(tx, path)
			if err != nil {
				return err
			}
			v := journal.Version{ID: m.Content.ID, Size: m.Content.Size, Directory: m.Content.Directory, Tombstone: m.Content.Tombstone}
			result = append(result, journal.Entry{Path: path, Next: v})
		}
		return nil
	})
	return result, err
}
