// Package journal serializes cooperative commits and persists prepared work,
// immutable version descriptions, ordered changes and per-client cursors. It
// performs no networking or content publication; a validated publisher is required.
package journal

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"
	"nas-sync/internal/stage"
)

const MaxEntries = 128
const MaxRecordBytes = 128 * 1024
const MaxPage = 32

var (
	ErrConflict      = errors.New("current head differs from expected base")
	ErrPending       = errors.New("prepared transaction requires recovery")
	ErrIdentity      = errors.New("operation or version identity reused")
	ErrFence         = errors.New("coordinator epoch changed")
	metaBucket       = []byte("meta-v1")
	headsBucket      = []byte("heads-v1")
	versionsBucket   = []byte("versions-v1")
	operationsBucket = []byte("operations-v1")
	changesBucket    = []byte("changes-v1")
	cursorsBucket    = []byte("cursors-v1")
)

// Version identifies immutable bytes retained separately from the visible file.
// Tombstones are explicit intent, never inferred from a failed/missing mount.
type Version struct {
	ID        string      `json:"id"`
	Size      int64       `json:"size"`
	Digest    hash.Digest `json:"digest"`
	Tombstone bool        `json:"tombstone,omitempty"`
	Directory bool        `json:"directory,omitempty"`
}

type Entry struct {
	Path     string  `json:"path"`
	Expected string  `json:"expected"` // empty means no recorded head
	Next     Version `json:"next"`
}
type Proposal struct {
	ID      string  `json:"id"`
	Client  string  `json:"client"`
	Entries []Entry `json:"entries"`
}
type Record struct {
	Proposal
	Sequence  uint64 `json:"sequence"`
	Epoch     uint64 `json:"epoch"`
	Committed bool   `json:"committed"`
}

// Publisher must idempotently verify retained immutable candidates and current
// visible bases, apply and durably flush the whole prepared batch, then return.
// On failure it must retain recovery data. It runs under coordinator ownership
// but outside a bbolt transaction. It must not reenter coordinator mutations.
// It is responsible for detecting/preserving noncooperating external edits.
type Publisher interface {
	Publish(context.Context, Record) error
}

type Coordinator struct {
	mu        sync.Mutex
	db        *bolt.DB
	store     *stage.Store
	epoch     uint64
	namespace string
	closed    bool
	changed   chan struct{}
}

func key(n uint64) []byte { var p [8]byte; binary.BigEndian.PutUint64(p[:], n); return p[:] }
func number(p []byte) (uint64, error) {
	if p == nil {
		return 0, nil
	}
	if len(p) != 8 {
		return 0, fmt.Errorf("invalid persistent sequence")
	}
	return binary.BigEndian.Uint64(p), nil
}

// Open takes an exclusive, lifetime native bbolt lock. A second independent
// process cannot open the same journal while this owner lives. Each successful
// open durably advances the epoch; remote clients cannot choose or renew it.
// This does not fence arbitrary SMB writers: all cooperative publication must
// run through this owner, on the NAS's native filesystem.
func Open(directory, namespace string) (*Coordinator, error) {
	if !manifest.ValidID(namespace) {
		return nil, fmt.Errorf("invalid journal namespace")
	}
	s, err := stage.Open(directory)
	if err != nil {
		return nil, err
	}
	db, err := bolt.Open("journal.db", 0600, &bolt.Options{Timeout: 150 * time.Millisecond,
		OpenFile: func(name string, flags int, mode os.FileMode) (*os.File, error) {
			if name != "journal.db" || flags != os.O_RDWR|os.O_CREATE || mode != 0600 {
				return nil, fmt.Errorf("unsupported journal file operation")
			}
			return s.OpenJournalFile()
		}})
	if err != nil {
		s.Close()
		return nil, err
	}
	c := &Coordinator{db: db, store: s, namespace: namespace, changed: make(chan struct{})}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{metaBucket, headsBucket, versionsBucket, operationsBucket, changesBucket, cursorsBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		m := tx.Bucket(metaBucket)
		if _, err := readBudget(tx); err != nil {
			return err
		}
		if prior := m.Get([]byte("namespace")); prior != nil && string(prior) != namespace {
			return fmt.Errorf("journal namespace mismatch")
		}
		if err := m.Put([]byte("namespace"), []byte(namespace)); err != nil {
			return err
		}
		epoch, err := number(m.Get([]byte("epoch")))
		if err != nil {
			return err
		}
		if epoch == ^uint64(0) {
			return fmt.Errorf("epoch exhausted")
		}
		c.epoch = epoch + 1
		return m.Put([]byte("epoch"), key(c.epoch))
	})
	if err == nil {
		err = s.Sync()
	}
	if err != nil {
		db.Close()
		s.Close()
		return nil, err
	}
	return c, nil
}
func (c *Coordinator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.changed)
	return errors.Join(c.db.Close(), c.store.Close())
}
func (c *Coordinator) Epoch() uint64     { return c.epoch }
func (c *Coordinator) Namespace() string { return c.namespace }

func validate(p Proposal) error {
	if !manifest.ValidID(p.ID) || !manifest.ValidID(p.Client) || len(p.Entries) < 1 || len(p.Entries) > MaxEntries {
		return fmt.Errorf("invalid proposal identity or entry count")
	}
	paths := make(map[string]bool, len(p.Entries))
	versions := make(map[string]bool, len(p.Entries))
	for _, e := range p.Entries {
		if err := validatePath(e.Path); err != nil {
			return err
		}
		if paths[e.Path] {
			return fmt.Errorf("duplicate proposal path")
		}
		paths[e.Path] = true
		if e.Expected != "" && !manifest.ValidID(e.Expected) {
			return fmt.Errorf("invalid expected base")
		}
		if e.Next.ID == e.Expected {
			return fmt.Errorf("invalid next version")
		}
		if err := ValidateVersion(e.Next); err != nil {
			return err
		}
		if versions[e.Next.ID] {
			return ErrIdentity
		}
		versions[e.Next.ID] = true
	}
	// A batch may not replace a path and one of its descendants. Publishing
	// directory/file transitions requires a separate explicit protocol.
	for path := range paths {
		for at := strings.LastIndexByte(path, '/'); at >= 0; at = strings.LastIndexByte(path, '/') {
			path = path[:at]
			if paths[path] {
				return fmt.Errorf("overlapping proposal paths")
			}
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(raw) > MaxRecordBytes-128 {
		return fmt.Errorf("proposal too large")
	}
	return nil
}

// ValidateProposal checks protocol structure before a transport stages content.
// Head/base checks and identity uniqueness still occur transactionally in Commit.
func ValidateProposal(p Proposal) error { return validate(p) }

func decode(raw []byte) (Record, error) {
	var r Record
	if len(raw) == 0 || len(raw) > MaxRecordBytes {
		return r, fmt.Errorf("invalid journal record size")
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if err := validate(r.Proposal); err != nil {
		return r, err
	}
	if r.Sequence == 0 || r.Epoch == 0 {
		return r, fmt.Errorf("invalid journal ordering")
	}
	return r, nil
}
func clone(r Record) Record { r.Entries = append([]Entry(nil), r.Entries...); return r }

func (c *Coordinator) check(tx *bolt.Tx, epoch uint64) error {
	stored, err := number(tx.Bucket(metaBucket).Get([]byte("epoch")))
	if err != nil {
		return err
	}
	if epoch != c.epoch || stored != epoch {
		return ErrFence
	}
	return nil
}

// Commit compares every head, durably prepares, invokes the publisher, then
// atomically commits all heads and the next journal sequence. Publication errors
// keep the prepared record and block new commits until Recover succeeds.
// A repeated operation with identical input returns its prior durable result.
func (c *Coordinator) Commit(ctx context.Context, epoch uint64, p Proposal, publisher Publisher) (Record, error) {
	if err := validate(p); err != nil {
		return Record{}, err
	}
	p.Entries = append([]Entry(nil), p.Entries...)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commitLocked(ctx, epoch, p, publisher)
}

func (c *Coordinator) commitLocked(ctx context.Context, epoch uint64, p Proposal, publisher Publisher) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if publisher == nil {
		return Record{}, fmt.Errorf("publisher required")
	}
	var r Record
	err := c.db.Update(func(tx *bolt.Tx) error {
		if err := c.check(tx, epoch); err != nil {
			return err
		}
		if tx.Bucket(metaBucket).Get(externalKey) != nil {
			return ErrPending
		}
		if raw := tx.Bucket(operationsBucket).Get([]byte(p.ID)); raw != nil {
			var err error
			r, err = decode(raw)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(r.Proposal, p) {
				return ErrIdentity
			}
			if !r.Committed {
				return ErrPending
			}
			return nil
		}
		m := tx.Bucket(metaBucket)
		if intake, err := readIntake(tx); err != nil {
			return err
		} else if intake != nil && !reflect.DeepEqual(*intake, p) {
			return ErrPending
		}
		if m.Get([]byte("pending")) != nil {
			return ErrPending
		}
		for _, e := range p.Entries {
			if string(tx.Bucket(headsBucket).Get([]byte(e.Path))) != e.Expected {
				return ErrConflict
			}
			if tx.Bucket(versionsBucket).Get([]byte(e.Next.ID)) != nil {
				return ErrIdentity
			}
		}
		if err := reservePublication(tx, p); err != nil {
			return err
		}
		seq, err := number(m.Get([]byte("sequence")))
		if err != nil {
			return err
		}
		if seq == ^uint64(0) {
			return fmt.Errorf("journal sequence exhausted")
		}
		r = Record{Proposal: p, Sequence: seq + 1, Epoch: epoch}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := tx.Bucket(operationsBucket).Put([]byte(p.ID), raw); err != nil {
			return err
		}
		if err := m.Delete(intakeKey); err != nil {
			return err
		}
		return m.Put([]byte("pending"), []byte(p.ID))
	})
	if err != nil {
		return Record{}, err
	}
	if r.Committed {
		return clone(r), nil
	}
	return c.publish(ctx, r, publisher)
}

func (c *Coordinator) publish(ctx context.Context, r Record, publisher Publisher) (Record, error) {
	if err := ctx.Err(); err != nil {
		return clone(r), err
	}
	if err := publisher.Publish(ctx, clone(r)); err != nil {
		return clone(r), err
	}
	// If canceled after publication, preserve pending work for idempotent replay.
	if err := ctx.Err(); err != nil {
		return clone(r), err
	}
	err := c.db.Update(func(tx *bolt.Tx) error {
		if err := c.check(tx, c.epoch); err != nil {
			return err
		}
		m := tx.Bucket(metaBucket)
		if string(m.Get([]byte("pending"))) != r.ID {
			return ErrPending
		}
		stored, err := decode(tx.Bucket(operationsBucket).Get([]byte(r.ID)))
		if err != nil {
			return err
		}
		if stored.Committed || !reflect.DeepEqual(stored, r) {
			return ErrIdentity
		}
		seq, err := number(m.Get([]byte("sequence")))
		if err != nil {
			return err
		}
		if seq+1 != r.Sequence {
			return fmt.Errorf("journal sequence gap")
		}
		for _, e := range r.Entries {
			if string(tx.Bucket(headsBucket).Get([]byte(e.Path))) != e.Expected {
				return ErrConflict
			}
			raw, err := json.Marshal(storedVersion{Version: e.Next, Path: e.Path})
			if err != nil {
				return err
			}
			if tx.Bucket(versionsBucket).Get([]byte(e.Next.ID)) != nil {
				return ErrIdentity
			}
			if err := tx.Bucket(versionsBucket).Put([]byte(e.Next.ID), raw); err != nil {
				return err
			}
			if err := tx.Bucket(headsBucket).Put([]byte(e.Path), []byte(e.Next.ID)); err != nil {
				return err
			}
		}
		r.Committed = true
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := tx.Bucket(operationsBucket).Put([]byte(r.ID), raw); err != nil {
			return err
		}
		if err := tx.Bucket(changesBucket).Put(key(r.Sequence), []byte(r.ID)); err != nil {
			return err
		}
		if err := m.Put([]byte("sequence"), key(r.Sequence)); err != nil {
			return err
		}
		return m.Delete([]byte("pending"))
	})
	if err != nil {
		r.Committed = false
	} else {
		c.notifyCommitted()
	}
	return clone(r), err
}

// Recover replays the single prepared batch under the current process's epoch.
// The original epoch in the record is provenance; the publisher must execute
// only here under current exclusive ownership, never from a stale client token.
func (c *Coordinator) Recover(ctx context.Context, epoch uint64, publisher Publisher) (*Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if publisher == nil {
		return nil, fmt.Errorf("publisher required")
	}
	var r *Record
	err := c.db.View(func(tx *bolt.Tx) error {
		if err := c.check(tx, epoch); err != nil {
			return err
		}
		id := tx.Bucket(metaBucket).Get([]byte("pending"))
		if id == nil {
			return nil
		}
		v, err := decode(tx.Bucket(operationsBucket).Get(id))
		if err != nil {
			return err
		}
		if v.Committed {
			return fmt.Errorf("committed pending record")
		}
		r = &v
		return nil
	})
	if err != nil || r == nil {
		return nil, err
	}
	done, err := c.publish(ctx, *r, publisher)
	return &done, err
}

// Head returns committed identity only; callers must defer visible-path access
// when pending is true, because a prepared publication can be partly applied.
func (c *Coordinator) Head(path string) (v *Version, pending bool, err error) {
	err = c.db.View(func(tx *bolt.Tx) error {
		pending = tx.Bucket(metaBucket).Get([]byte("pending")) != nil
		id := tx.Bucket(headsBucket).Get([]byte(path))
		if id == nil {
			return nil
		}
		value, err := readVersion(tx, string(id))
		if err != nil {
			return err
		}
		if value.Path != "" && value.Path != path {
			return ErrIdentity
		}
		v = &value.Version
		return nil
	})
	return
}

// Version returns a committed immutable description for publication/recovery.
// It does not open content and is safe inside a publisher callback.
func (c *Coordinator) Version(id string) (*Version, error) {
	if !manifest.ValidID(id) {
		return nil, fmt.Errorf("invalid version ID")
	}
	var v *Version
	err := c.db.View(func(tx *bolt.Tx) error {
		got, err := readVersion(tx, id)
		if err != nil {
			return err
		}
		v = &got.Version
		return nil
	})
	return v, err
}

// Operation retrieves an immutable proposal/result for scoped retry/recovery.
func (c *Coordinator) Operation(id string) (*Record, error) {
	if !manifest.ValidID(id) {
		return nil, fmt.Errorf("invalid operation ID")
	}
	var r *Record
	err := c.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(operationsBucket).Get([]byte(id))
		if raw == nil {
			return nil
		}
		v, err := decode(raw)
		if err != nil {
			return err
		}
		r = &v
		return nil
	})
	return r, err
}

// Changes returns a bounded, contiguous page of committed batches. It never
// advances a client cursor, skips a missing record, or reads file contents.
func (c *Coordinator) Changes(after uint64, limit int) ([]Record, error) {
	if limit < 1 || limit > MaxPage {
		return nil, fmt.Errorf("journal page must be in [1,%d]", MaxPage)
	}
	out := make([]Record, 0, limit)
	err := c.db.View(func(tx *bolt.Tx) error {
		seq, err := number(tx.Bucket(metaBucket).Get([]byte("sequence")))
		if err != nil {
			return err
		}
		if after > seq {
			return fmt.Errorf("cursor beyond journal head")
		}
		for n := after; n < seq && len(out) < limit; {
			n++
			id := tx.Bucket(changesBucket).Get(key(n))
			if id == nil {
				return fmt.Errorf("journal gap at %d", n)
			}
			r, err := decode(tx.Bucket(operationsBucket).Get(id))
			if err != nil {
				return err
			}
			if !r.Committed || r.Sequence != n {
				return fmt.Errorf("invalid committed journal ordering")
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

func (c *Coordinator) Cursor(client string) (uint64, error) {
	if !manifest.ValidID(client) {
		return 0, fmt.Errorf("invalid client ID")
	}
	var n uint64
	err := c.db.View(func(tx *bolt.Tx) error {
		var err error
		n, err = number(tx.Bucket(cursorsBucket).Get([]byte(client)))
		return err
	})
	return n, err
}

// Acknowledge advances exactly one committed batch after the client has durably
// applied or recorded every entry (including conflict/exclusion deferrals).
// The authenticated transport must bind client to the caller's identity.
func (c *Coordinator) Acknowledge(client string, expected, next uint64) error {
	if !manifest.ValidID(client) || expected == ^uint64(0) || next != expected+1 {
		return fmt.Errorf("invalid cursor advancement")
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		if policies := tx.Bucket(ackPoliciesBucket); policies != nil && policies.Get([]byte(client)) != nil {
			return fmt.Errorf("policy-bound cursor requires range acknowledgement")
		}
		b := tx.Bucket(cursorsBucket)
		current, err := number(b.Get([]byte(client)))
		if err != nil {
			return err
		}
		if current == next {
			return nil
		}
		if current != expected {
			return ErrConflict
		}
		if tx.Bucket(changesBucket).Get(key(next)) != nil {
			return b.Put([]byte(client), key(next))
		}
		return fmt.Errorf("cannot acknowledge uncommitted sequence")
	})
}
