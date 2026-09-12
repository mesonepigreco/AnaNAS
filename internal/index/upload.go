package index

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

const MaxUploadBytes = journal.MaxRecordBytes + 32*1024

var uploadKey = []byte("upload-outbox-v1")
var lastUploadKey = []byte("last-upload-v1")
var replicaNamespaceKey = []byte("replica-namespace-v1")

// ReplicaNamespace reads the existing upload/inbox target binding without
// creating it. Empty means this index has not yet received or sent replica work.
func (d *DB) ReplicaNamespace() (string, error) {
	var namespace string
	err := d.db.View(func(tx *bolt.Tx) error {
		namespace = string(tx.Bucket(meta).Get(replicaNamespaceKey))
		if namespace != "" && !manifest.ValidID(namespace) {
			return fmt.Errorf("invalid replica namespace binding")
		}
		return nil
	})
	return namespace, err
}

func bindReplicaNamespace(tx *bolt.Tx, namespace string) error {
	prior := tx.Bucket(meta).Get(replicaNamespaceKey)
	if prior != nil {
		if string(prior) != namespace {
			return fmt.Errorf("replica namespace requires reconciliation")
		}
		return nil
	}
	remote, err := readRemoteState(tx)
	if err != nil {
		return err
	}
	if remote.Namespace != "" && remote.Namespace != namespace {
		return fmt.Errorf("upload target differs from received namespace")
	}
	return tx.Bucket(meta).Put(replicaNamespaceKey, []byte(namespace))
}

type UploadReceipt struct {
	Sequence uint64 `json:"sequence"`
	Epoch    uint64 `json:"epoch"`
}

// Upload retains the exact operation identity, observed generations and wire
// lengths across restart. Content/delta spools live separately in private native
// storage and must remain pinned while this record exists.
type Upload struct {
	Namespace    string           `json:"namespace"`
	Proposal     journal.Proposal `json:"proposal"`
	Generations  []uint64         `json:"generations"`
	DeltaBytes   []int64          `json:"deltaBytes"`
	DeltaDigests []hash.Digest    `json:"deltaDigests"`
	Receipt      *UploadReceipt   `json:"receipt,omitempty"`
}

func (u Upload) Validate() error {
	if !manifest.ValidID(u.Namespace) {
		return fmt.Errorf("upload namespace required")
	}
	if err := journal.ValidateProposal(u.Proposal); err != nil {
		return err
	}
	if len(u.Generations) != len(u.Proposal.Entries) || len(u.DeltaBytes) != len(u.Generations) || len(u.DeltaDigests) != len(u.Generations) {
		return fmt.Errorf("upload metadata count differs")
	}
	var total int64
	for i, e := range u.Proposal.Entries {
		if u.Generations[i] == 0 {
			return fmt.Errorf("upload generation required")
		}
		if e.Next.Size > (8<<30)-total {
			return fmt.Errorf("upload content exceeds batch bound")
		}
		total += e.Next.Size
		if e.Next.Directory || e.Next.Tombstone {
			if u.DeltaBytes[i] != 0 || u.DeltaDigests[i] != (hash.Digest{}) {
				return fmt.Errorf("metadata upload contains delta")
			}
		} else if u.DeltaDigests[i] == (hash.Digest{}) || u.DeltaBytes[i] < delta.HeaderBytes+33 || u.DeltaBytes[i] > e.Next.Size+int64(delta.MaxOperations)*45+delta.HeaderBytes+33 {
			return fmt.Errorf("upload wire length exceeds bounds")
		}
	}
	if u.Receipt != nil && (u.Receipt.Sequence == 0 || u.Receipt.Epoch == 0) {
		return fmt.Errorf("invalid upload receipt")
	}
	raw, err := json.Marshal(u)
	limit := MaxUploadBytes
	if u.Receipt == nil {
		limit -= 128
	} // reserve room for durable commit metadata
	if err != nil || len(raw) > limit {
		return fmt.Errorf("upload metadata exceeds bound")
	}
	return nil
}

func readUpload(tx *bolt.Tx) (*Upload, error) {
	raw := tx.Bucket(meta).Get(uploadKey)
	if raw == nil {
		return nil, nil
	}
	if len(raw) > MaxUploadBytes {
		return nil, fmt.Errorf("persistent upload exceeds bound")
	}
	var u Upload
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	if err := u.Validate(); err != nil {
		return nil, err
	}
	if string(tx.Bucket(meta).Get([]byte("client-id"))) != u.Proposal.Client {
		return nil, fmt.Errorf("persistent upload client differs")
	}
	return &u, nil
}

func (d *DB) PendingUpload() (*Upload, error) {
	var u *Upload
	err := d.db.View(func(tx *bolt.Tx) error { var err error; u, err = readUpload(tx); return err })
	return u, err
}

// PrepareUpload must complete before any upload request. It reserves one batch
// after checking current generations, bases, exclusions and pause/conflict state.
// Identical retry preserves its original identity, even if newer edits exist.
func (d *DB) PrepareUpload(u Upload) error {
	if !d.uploadMu.TryLock() {
		return fmt.Errorf("upload preparation busy")
	}
	defer d.uploadMu.Unlock()
	return d.prepareUpload(u, nil)
}

func (d *DB) prepareUpload(u Upload, preparation *UploadPreparation) error {
	if u.Receipt != nil {
		return fmt.Errorf("new upload cannot assert a commit receipt")
	}
	if err := u.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		pending, err := readUploadPreparation(tx)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(pending, preparation) {
			return ErrStale
		}
		if preparation != nil {
			if err := preparation.matches(u); err != nil {
				return err
			}
		}
		if err := bindReplicaNamespace(tx, u.Namespace); err != nil {
			return err
		}
		if string(tx.Bucket(meta).Get([]byte("client-id"))) != u.Proposal.Client {
			return fmt.Errorf("upload client differs from index")
		}
		old, err := readUpload(tx)
		if err != nil {
			return err
		}
		if old != nil {
			old.Receipt = nil
			if reflect.DeepEqual(*old, u) {
				return nil
			}
			return ErrStale
		}
		if string(tx.Bucket(meta).Get(lastUploadKey)) == u.Proposal.ID {
			return ErrStale
		}
		if paused := tx.Bucket(meta).Get([]byte("paused")); paused != nil && string(paused) != "0" {
			return fmt.Errorf("upload paused or pause state invalid")
		}
		for i, e := range u.Proposal.Entries {
			var r Record
			if err := json.Unmarshal(tx.Bucket(files).Get([]byte(e.Path)), &r); err != nil {
				return ErrStale
			}
			if r.Excluded || r.Generation != u.Generations[i] {
				return ErrStale
			}
			mode := os.FileMode(r.Fingerprint.Mode)
			if r.Missing != e.Next.Tombstone || (!r.Missing && (mode.IsDir() != e.Next.Directory || (!e.Next.Directory && (!mode.IsRegular() || r.Fingerprint.Size != e.Next.Size)))) {
				return fmt.Errorf("upload type or size differs from observed generation")
			}
			base, err := readBase(tx, e.Path)
			if err != nil {
				return err
			}
			if baseID(base) != e.Expected {
				return ErrStale
			}
			if e.Next.Tombstone && base == nil {
				return fmt.Errorf("deletion requires an acknowledged base")
			}
			state, err := readPathState(tx, e.Path)
			if err != nil {
				return err
			}
			if state.Paused || state.Conflict != nil {
				return fmt.Errorf("upload path paused or conflicted")
			}
		}
		if err := tx.Bucket(meta).Put(uploadKey, raw); err != nil {
			return err
		}
		return tx.Bucket(meta).Delete(uploadPreparationKey)
	})
}

// RecordUploadCommit accepts only the exact authenticated remote result for the
// pending operation. It does not acknowledge local bases or clear dirty work.
func (d *DB) RecordUploadCommit(namespace string, record journal.Record) error {
	if !record.Committed || record.Sequence == 0 || record.Epoch == 0 {
		return fmt.Errorf("durable committed upload result required")
	}
	if err := journal.ValidateProposal(record.Proposal); err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		u, err := readUpload(tx)
		if err != nil {
			return err
		}
		if u == nil || u.Namespace != namespace || !reflect.DeepEqual(u.Proposal, record.Proposal) {
			return ErrStale
		}
		receipt := UploadReceipt{Sequence: record.Sequence, Epoch: record.Epoch}
		if u.Receipt != nil {
			if *u.Receipt == receipt {
				return nil
			}
			return ErrStale
		}
		u.Receipt = &receipt
		raw, err := json.Marshal(u)
		if err != nil {
			return err
		}
		return tx.Bucket(meta).Put(uploadKey, raw)
	})
}

// CompleteUpload releases metadata only after matching durable local bases
// have been recorded. Spool cleanup remains a separate quota/retention action.
// Acknowledge's generation check preserves local edits newer than this upload.
func (d *DB) CompleteUpload(operation string) error {
	if !manifest.ValidID(operation) {
		return fmt.Errorf("invalid upload operation")
	}
	return d.db.Update(func(tx *bolt.Tx) error {
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
		if u.Proposal.ID != operation || u.Receipt == nil {
			return ErrStale
		}
		for _, e := range u.Proposal.Entries {
			base, err := readBase(tx, e.Path)
			if err != nil {
				return err
			}
			v := e.Next
			if base == nil || base.Content.ID != v.ID || base.Content.Size != v.Size || base.Content.Tombstone != v.Tombstone || base.Content.Directory != v.Directory {
				return ErrStale
			}
		}
		if err := tx.Bucket(meta).Put(lastUploadKey, []byte(operation)); err != nil {
			return err
		}
		return tx.Bucket(meta).Delete(uploadKey)
	})
}
