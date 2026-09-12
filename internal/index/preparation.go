package index

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

var uploadPreparationKey = []byte("upload-preparation-v1")

// UploadSource claims an unused candidate ID before snapshot or wire creation.
// Observed metadata identifies the source generation, not trusted content bytes.
type UploadSource struct {
	Path, Expected, ID string
	Generation         uint64
	Fingerprint        Fingerprint
	Missing            bool
}

type UploadPreparation struct {
	Namespace, ID, Client string
	Sources               []UploadSource
}

func (p UploadPreparation) Validate() error {
	if !manifest.ValidID(p.Namespace) || len(p.Sources) < 1 || len(p.Sources) > journal.MaxEntries {
		return fmt.Errorf("bounded upload preparation required")
	}
	proposal := journal.Proposal{ID: p.ID, Client: p.Client}
	var total int64
	for _, s := range p.Sources {
		mode := os.FileMode(s.Fingerprint.Mode)
		if s.Generation == 0 || (!s.Missing && !mode.IsDir() && !mode.IsRegular()) || (s.Missing && s.Expected == "") {
			return fmt.Errorf("invalid observed upload source")
		}
		v := journal.Version{ID: s.ID, Tombstone: s.Missing, Directory: !s.Missing && mode.IsDir()}
		if !v.Tombstone && !v.Directory {
			v.Size = s.Fingerprint.Size
			if v.Size < 0 || v.Size > (8<<30)-total {
				return fmt.Errorf("upload preparation exceeds content bound")
			}
			total += v.Size
			// Only structural validation uses this placeholder. No digest is
			// trusted or sent until the immutable snapshot has been verified.
			v.Digest = hash.SumBytes(nil)
		}
		proposal.Entries = append(proposal.Entries, journal.Entry{Path: s.Path, Expected: s.Expected, Next: v})
	}
	if err := journal.ValidateProposal(proposal); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > MaxUploadBytes {
		return fmt.Errorf("upload preparation metadata exceeds bound")
	}
	return nil
}

func readUploadPreparation(tx *bolt.Tx) (*UploadPreparation, error) {
	raw := tx.Bucket(meta).Get(uploadPreparationKey)
	if raw == nil {
		return nil, nil
	}
	if len(raw) > MaxUploadBytes {
		return nil, fmt.Errorf("upload preparation exceeds bound")
	}
	var p UploadPreparation
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if string(tx.Bucket(meta).Get([]byte("client-id"))) != p.Client {
		return nil, ErrStale
	}
	return &p, nil
}

func (d *DB) PendingUploadPreparation() (*UploadPreparation, error) {
	var p *UploadPreparation
	err := d.db.View(func(tx *bolt.Tx) error { var err error; p, err = readUploadPreparation(tx); return err })
	return p, err
}

func (p UploadPreparation) matches(u Upload) error {
	if u.Namespace != p.Namespace || u.Proposal.ID != p.ID || u.Proposal.Client != p.Client || len(u.Proposal.Entries) != len(p.Sources) {
		return ErrStale
	}
	for i, s := range p.Sources {
		e := u.Proposal.Entries[i]
		if e.Path != s.Path || e.Expected != s.Expected || e.Next.ID != s.ID || u.Generations[i] != s.Generation || e.Next.Tombstone != s.Missing || e.Next.Directory != (!s.Missing && os.FileMode(s.Fingerprint.Mode).IsDir()) {
			return ErrStale
		}
		if !e.Next.Directory && !e.Next.Tombstone && e.Next.Size != s.Fingerprint.Size {
			return ErrStale
		}
	}
	return nil
}

func validateUploadSources(tx *bolt.Tx, p UploadPreparation) error {
	if err := bindReplicaNamespace(tx, p.Namespace); err != nil {
		return err
	}
	if string(tx.Bucket(meta).Get([]byte("client-id"))) != p.Client {
		return ErrStale
	}
	if value := tx.Bucket(meta).Get([]byte("paused")); value != nil && string(value) != "0" {
		return fmt.Errorf("upload preparation paused")
	}
	if tx.Bucket(meta).Get(uploadKey) != nil || tx.Bucket(meta).Get(uploadPreparationKey) != nil || string(tx.Bucket(meta).Get(lastUploadKey)) == p.ID {
		return ErrStale
	}
	for _, s := range p.Sources {
		var r Record
		if err := json.Unmarshal(tx.Bucket(files).Get([]byte(s.Path)), &r); err != nil {
			return ErrStale
		}
		if !r.Dirty || r.Excluded || r.Generation != s.Generation || r.Fingerprint != s.Fingerprint || r.Missing != s.Missing {
			return ErrStale
		}
		base, err := readBase(tx, s.Path)
		if err != nil {
			return err
		}
		if baseID(base) != s.Expected {
			return ErrStale
		}
		state, err := readPathState(tx, s.Path)
		if err != nil {
			return err
		}
		if state.Paused || state.Conflict != nil {
			return fmt.Errorf("upload source paused or conflicted")
		}
	}
	return nil
}

// BuildUpload serializes preparation/abort/direct outbox creation. It durably
// claims exact vacant IDs before calling build outside the database transaction.
// Failure retains provenance and blocks another preparation until explicit abort.
// The caller also holds the local journal's lifetime lock, checks unused version
// identities, and serializes all replica work. No transfer is permitted here.
func (d *DB) BuildUpload(ctx context.Context, p UploadPreparation, vacant func() error, build func() (Upload, error)) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if vacant == nil || build == nil {
		return fmt.Errorf("vacancy and builder required")
	}
	p.Sources = append([]UploadSource(nil), p.Sources...)
	if !d.uploadMu.TryLock() {
		return fmt.Errorf("upload preparation busy")
	}
	defer d.uploadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := vacant(); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := d.db.Update(func(tx *bolt.Tx) error {
		if err := validateUploadSources(tx, p); err != nil {
			return err
		}
		return tx.Bucket(meta).Put(uploadPreparationKey, raw)
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	u, err := build()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.prepareUpload(u, &p)
}

// AbortUploadPreparation invokes exact-ID cleanup before forgetting provenance.
// Cleanup must prove these IDs are uncommitted and not held by any live worker.
// It must flush removals and tolerate an interrupted earlier abort. A cleanup
// error retains this record. No observer/base/dirty record is changed.
func (d *DB) AbortUploadPreparation(cleanup func(UploadPreparation) error) error {
	if cleanup == nil {
		return fmt.Errorf("scoped cleanup required")
	}
	if !d.uploadMu.TryLock() {
		return fmt.Errorf("upload preparation busy")
	}
	defer d.uploadMu.Unlock()
	p, err := d.PendingUploadPreparation()
	if err != nil || p == nil {
		return err
	}
	u, err := d.PendingUpload()
	if err != nil {
		return err
	}
	if u != nil {
		return ErrStale
	}
	copy := *p
	copy.Sources = append([]UploadSource(nil), p.Sources...)
	if err := cleanup(copy); err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		current, err := readUploadPreparation(tx)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, p) || tx.Bucket(meta).Get(uploadKey) != nil {
			return ErrStale
		}
		return tx.Bucket(meta).Delete(uploadPreparationKey)
	})
}
