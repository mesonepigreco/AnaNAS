package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

var remoteInbox = []byte("remote-inbox-v1")
var remoteStateKey = []byte("remote-state-v1")

// Received means durably queued, not synchronized. Completed means every entry
// has durable applied/conflict/exclusion bookkeeping; it is not a content count.
type RemoteState struct {
	Namespace       string `json:"namespace"`
	Policy          string `json:"policy"`
	Received        uint64 `json:"received"`
	Completed       uint64 `json:"completed"`
	Acknowledged    uint64 `json:"acknowledged"`
	ExcludedEntries uint64 `json:"excludedEntries"`
	LastPage        string `json:"lastPage"`
}

func remoteKey(sequence uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], sequence)
	return b[:]
}

func readRemoteState(tx *bolt.Tx) (RemoteState, error) {
	var s RemoteState
	raw := tx.Bucket(meta).Get(remoteStateKey)
	if raw == nil {
		return s, nil
	}
	if len(raw) > 4096 {
		return s, fmt.Errorf("remote state exceeds limit")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if !manifest.ValidID(s.Namespace) || !manifest.ValidID(s.Policy) || !manifest.ValidID(s.LastPage) || s.Acknowledged > s.Completed || s.Completed > s.Received || s.Received-s.Completed > journal.MaxPage {
		return s, fmt.Errorf("invalid persistent remote state")
	}
	return s, nil
}

func (d *DB) RemoteState() (RemoteState, error) {
	var s RemoteState
	err := d.db.View(func(tx *bolt.Tx) error { var err error; s, err = readRemoteState(tx); return err })
	return s, err
}

// ReceiveRemotePage atomically persists one bounded page and its receive cursor.
// Another page cannot arrive until the current inbox is completed. A namespace
// or policy change fails closed; reconciliation must not reuse the old cursor.
// This method neither acknowledges the server nor clears local dirty work.
func (d *DB) ReceiveRemotePage(page changefeed.Page) error {
	if err := page.Validate(page.After, journal.MaxPage); err != nil {
		return err
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	identity := hex.EncodeToString(digest[:])
	return d.db.Update(func(tx *bolt.Tx) error {
		if err := bindReplicaNamespace(tx, page.Namespace); err != nil {
			return err
		}
		s, err := readRemoteState(tx)
		if err != nil {
			return err
		}
		if s.Namespace != "" && (s.Namespace != page.Namespace || s.Policy != page.Policy) {
			return fmt.Errorf("remote namespace or policy requires reconciliation")
		}
		if s.LastPage == identity && s.Received == page.Through {
			return nil
		}
		if s.Received != page.After || s.Completed != s.Received {
			return ErrStale
		}
		b, err := tx.CreateBucketIfNotExists(remoteInbox)
		if err != nil {
			return err
		}
		if key, _ := b.Cursor().First(); key != nil {
			return fmt.Errorf("inbox differs from completed cursor")
		}
		for _, batch := range page.Batches {
			data, err := json.Marshal(batch)
			if err != nil {
				return err
			}
			if err := b.Put(remoteKey(batch.Sequence), data); err != nil {
				return err
			}
		}
		s.Namespace, s.Policy, s.Received, s.LastPage = page.Namespace, page.Policy, page.Through, identity
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return tx.Bucket(meta).Put(remoteStateKey, data)
	})
}

func nextRemote(tx *bolt.Tx, s RemoteState) (*changefeed.Batch, error) {
	if s.Completed == s.Received {
		return nil, nil
	}
	b := tx.Bucket(remoteInbox)
	if b == nil {
		return nil, fmt.Errorf("missing remote inbox")
	}
	raw := b.Get(remoteKey(s.Completed + 1))
	if len(raw) == 0 || len(raw) > journal.MaxRecordBytes {
		return nil, fmt.Errorf("missing or oversized remote batch")
	}
	var batch changefeed.Batch
	if err := json.Unmarshal(raw, &batch); err != nil {
		return nil, err
	}
	page := changefeed.Page{Namespace: s.Namespace, Policy: s.Policy, After: s.Completed, Through: s.Completed + 1, Batches: []changefeed.Batch{batch}}
	if err := page.Validate(s.Completed, 1); err != nil {
		return nil, err
	}
	return &batch, nil
}

// NextRemoteBatch returns only the oldest pending batch, detached from bbolt.
func (d *DB) NextRemoteBatch() (*changefeed.Batch, error) {
	var batch *changefeed.Batch
	err := d.db.View(func(tx *bolt.Tx) error {
		s, err := readRemoteState(tx)
		if err != nil {
			return err
		}
		batch, err = nextRemote(tx, s)
		return err
	})
	return batch, err
}

// CompleteRemoteBatch requires durable matching bases or unresolved conflict
// identities for every visible entry. Receipt alone cannot satisfy this check.
// Directory bases must match their explicit type; an ordinary empty-file
// manifest cannot complete a directory entry. No content is read here.
func (d *DB) CompleteRemoteBatch(sequence uint64, operation string) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		s, err := readRemoteState(tx)
		if err != nil {
			return err
		}
		batch, err := nextRemote(tx, s)
		if err != nil {
			return err
		}
		if batch == nil || batch.Sequence != sequence || batch.ID != operation {
			return ErrStale
		}
		for _, entry := range batch.Entries {
			base, err := readBase(tx, entry.Path)
			if err != nil {
				return err
			}
			v := entry.Next
			if base != nil && base.Content.ID == v.ID && base.Content.Size == v.Size && base.Content.Tombstone == v.Tombstone && base.Content.Directory == v.Directory {
				continue
			}
			state, err := readPathState(tx, entry.Path)
			if err != nil {
				return err
			}
			if state.Conflict == nil || state.Conflict.RemoteID != v.ID {
				return ErrStale
			}
		}
		return completeRemote(tx, s, batch)
	})
}

func completeRemote(tx *bolt.Tx, s RemoteState, batch *changefeed.Batch) error {
	if ^uint64(0)-s.ExcludedEntries < uint64(batch.Excluded) {
		return fmt.Errorf("excluded entry counter exhausted")
	}
	s.ExcludedEntries += uint64(batch.Excluded)
	s.Completed = batch.Sequence
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := tx.Bucket(meta).Put(remoteStateKey, data); err != nil {
		return err
	}
	return tx.Bucket(remoteInbox).Delete(remoteKey(batch.Sequence))
}
