package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/hash"
)

var externalKey = []byte("external-capture-v1")

func externalShape(p Proposal) Proposal {
	p.Entries = append([]Entry(nil), p.Entries...)
	for i := range p.Entries {
		if !p.Entries[i].Next.Directory && !p.Entries[i].Next.Tombstone {
			p.Entries[i].Next.Digest = hash.SumBytes(nil)
		}
	}
	return p
}

// PendingExternal returns the one locally owned capture, if any. Remote intake
// remains separate and must never be interpreted as a local visible-file edit.
func (c *Coordinator) PendingExternal() (*Proposal, error) {
	var p *Proposal
	err := c.db.View(func(tx *bolt.Tx) error {
		marker := tx.Bucket(metaBucket).Get(externalKey)
		if marker == nil {
			return nil
		}
		var err error
		p, err = readIntake(tx)
		if err != nil {
			return err
		}
		if p == nil || len(marker) != 72 || string(marker[:64]) != p.ID {
			return ErrIdentity
		}
		return nil
	})
	return p, err
}

// ImportExternal snapshots one observed native entry into the ordinary change
// journal without rewriting its visible path. The trusted capture callback runs
// under coordinator ownership, after durable space/ID reservation. It must seal
// and verify immutable bytes and may change only the regular-file digest. On a
// retry it may reuse that owned snapshot, even if the visible file changed again.
// No publication phase is created; all final metadata becomes durable together.
func (c *Coordinator) ImportExternal(ctx context.Context, epoch uint64, p Proposal, capture func(context.Context, bool) (Version, error)) (Record, error) {
	if err := validate(p); err != nil {
		return Record{}, err
	}
	if len(p.Entries) != 1 || capture == nil {
		return Record{}, fmt.Errorf("one observed entry and native capture required")
	}
	p = externalShape(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	var existing *Record
	resume := false
	err := c.db.Update(func(tx *bolt.Tx) error {
		if err := c.check(tx, epoch); err != nil {
			return err
		}
		u, err := readBudget(tx)
		if err != nil {
			return err
		}
		if u == nil || u.ReplicaNamespace != "" {
			return fmt.Errorf("external capture requires a native publication budget")
		}
		if raw := tx.Bucket(operationsBucket).Get([]byte(p.ID)); raw != nil {
			r, err := decode(raw)
			if err != nil {
				return err
			}
			if !r.Committed || !reflect.DeepEqual(externalShape(r.Proposal), p) {
				return ErrIdentity
			}
			existing = &r
			return nil
		}
		m := tx.Bucket(metaBucket)
		if m.Get([]byte("pending")) != nil {
			return ErrPending
		}
		prior, err := readIntake(tx)
		if err != nil {
			return err
		}
		marker := m.Get(externalKey)
		if prior != nil {
			if !reflect.DeepEqual(*prior, p) || len(marker) != 72 || string(marker[:64]) != p.ID {
				return ErrPending
			}
			resume = true
		} else if marker != nil {
			return ErrIdentity
		}
		e := p.Entries[0]
		if string(tx.Bucket(headsBucket).Get([]byte(e.Path))) != e.Expected {
			return ErrConflict
		}
		if tx.Bucket(versionsBucket).Get([]byte(e.Next.ID)) != nil {
			return ErrIdentity
		}
		if !resume {
			if err := c.store.RequireExternalVacant(e.Next.ID); err != nil {
				return err
			}
		}
		if err := reservePublication(tx, p); err != nil {
			return err
		}
		if resume {
			return nil
		}
		charge, err := publicationCharge(tx, p)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if err := m.Put(intakeKey, raw); err != nil {
			return err
		}
		return m.Put(externalKey, append([]byte(p.ID), key(uint64(charge))...))
	})
	if err != nil {
		return Record{}, err
	}
	if existing != nil {
		return clone(*existing), nil
	}
	v, err := capture(ctx, resume)
	if err != nil {
		return Record{}, err
	}
	actual := clone(Record{Proposal: p}).Proposal
	actual.Entries[0].Next = v
	if err := validate(actual); err != nil {
		return Record{}, err
	}
	if !reflect.DeepEqual(externalShape(actual), p) {
		return Record{}, ErrIdentity
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	var result Record
	err = c.db.Update(func(tx *bolt.Tx) error {
		if err := c.check(tx, epoch); err != nil {
			return err
		}
		m := tx.Bucket(metaBucket)
		seq, err := number(m.Get([]byte("sequence")))
		if err != nil {
			return err
		}
		if seq == ^uint64(0) {
			return fmt.Errorf("journal sequence exhausted")
		}
		result = Record{Proposal: actual, Sequence: seq + 1, Epoch: epoch, Committed: true}
		e := actual.Entries[0]
		raw, err := json.Marshal(storedVersion{Version: e.Next, Path: e.Path})
		if err != nil {
			return err
		}
		if err := tx.Bucket(versionsBucket).Put([]byte(e.Next.ID), raw); err != nil {
			return err
		}
		if err := tx.Bucket(headsBucket).Put([]byte(e.Path), []byte(e.Next.ID)); err != nil {
			return err
		}
		raw, err = json.Marshal(result)
		if err != nil {
			return err
		}
		if err := tx.Bucket(operationsBucket).Put([]byte(p.ID), raw); err != nil {
			return err
		}
		if err := tx.Bucket(changesBucket).Put(key(result.Sequence), []byte(p.ID)); err != nil {
			return err
		}
		raw, err = json.Marshal(actual)
		if err != nil {
			return err
		}
		digest := hash.SumBytes(raw)
		if err := tx.Bucket(reservationsBucket).Put([]byte(p.ID), digest[:]); err != nil {
			return err
		}
		if err := m.Put([]byte("sequence"), key(result.Sequence)); err != nil {
			return err
		}
		if err := m.Delete(intakeKey); err != nil {
			return err
		}
		return m.Delete(externalKey)
	})
	if err == nil {
		c.notifyCommitted()
	}
	return clone(result), err
}

// AbortExternal discards only the durably claimed, never-committed local capture.
// Failed cleanup preserves the reservation/intake. The visible file is untouched.
func (c *Coordinator) AbortExternal(ctx context.Context, epoch uint64, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var p *Proposal
	var charge uint64
	err := c.db.View(func(tx *bolt.Tx) error {
		if err := c.check(tx, epoch); err != nil {
			return err
		}
		m := tx.Bucket(metaBucket)
		marker := m.Get(externalKey)
		if len(marker) != 72 || string(marker[:64]) != id || m.Get([]byte("pending")) != nil {
			return ErrPending
		}
		var err error
		p, err = readIntake(tx)
		if err != nil {
			return err
		}
		if p == nil || p.ID != id || len(p.Entries) != 1 || tx.Bucket(operationsBucket).Get([]byte(id)) != nil {
			return ErrIdentity
		}
		if tx.Bucket(versionsBucket).Get([]byte(p.Entries[0].Next.ID)) != nil {
			return ErrIdentity
		}
		charge, err = number(marker[64:])
		return err
	})
	if err != nil {
		return err
	}
	if err := c.store.AbortExternal(p.Entries[0].Next.ID); err != nil {
		return err
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		u, err := readBudget(tx)
		if err != nil {
			return err
		}
		if u == nil || charge > uint64(u.ReservedBytes) || u.Entries < 1 {
			return ErrIdentity
		}
		u.ReservedBytes -= int64(charge)
		u.Entries--
		if err := updateBudget(tx, u); err != nil {
			return err
		}
		if err := tx.Bucket(reservationsBucket).Delete([]byte(id)); err != nil {
			return err
		}
		if err := tx.Bucket(metaBucket).Delete(intakeKey); err != nil {
			return err
		}
		return tx.Bucket(metaBucket).Delete(externalKey)
	})
}
