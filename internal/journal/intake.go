package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/stage"
)

var intakeKey = []byte("upload-intake-v1")

// CheckStore binds transport staging to the exact directory protected by this
// coordinator. A private directory alone does not establish intake ownership.
func (c *Coordinator) CheckStore(store *stage.Store) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("coordinator closed")
	}
	same, err := c.store.SameDirectory(store)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("staging store differs from coordinator directory")
	}
	return nil
}

func readIntake(tx *bolt.Tx) (*Proposal, error) {
	raw := tx.Bucket(metaBucket).Get(intakeKey)
	if raw == nil {
		return nil, nil
	}
	if len(raw) > MaxRecordBytes {
		return nil, fmt.Errorf("oversized upload intake")
	}
	var p Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if err := validate(p); err != nil {
		return nil, err
	}
	return &p, nil
}

// StageAndCommit owns one upload from durable intake through publication. A new
// intake requires vacant candidate names. An identical retry may verify/reuse
// ready files and discard its abandoned partials while the callback holds this
// owner's mutex and lifetime database lock. The callback must not reenter journal
// mutations or publish visible files; nil asserts all candidate bytes verified.
// Failed intake remains durable and blocks other new commits, without appearing
// as prepared publication or advancing a journal sequence. No age-based abort or
// unowned-artifact adoption is performed.
func (c *Coordinator) StageAndCommit(ctx context.Context, epoch uint64, p Proposal, receive func(context.Context, bool) error, publisher Publisher) (Record, error) {
	if err := validate(p); err != nil {
		return Record{}, err
	}
	if receive == nil || publisher == nil {
		return Record{}, fmt.Errorf("receiver and publisher required")
	}
	p.Entries = append([]Entry(nil), p.Entries...)
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
		if tx.Bucket(metaBucket).Get(externalKey) != nil {
			return ErrPending
		}
		if raw := tx.Bucket(operationsBucket).Get([]byte(p.ID)); raw != nil {
			r, err := decode(raw)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(r.Proposal, p) {
				return ErrIdentity
			}
			if !r.Committed {
				return ErrPending
			}
			existing = &r
			return nil
		}
		if tx.Bucket(metaBucket).Get([]byte("pending")) != nil {
			return ErrPending
		}
		prior, err := readIntake(tx)
		if err != nil {
			return err
		}
		if prior != nil {
			if prior.ID == p.ID && !reflect.DeepEqual(*prior, p) {
				return ErrIdentity
			}
			if !reflect.DeepEqual(*prior, p) {
				return ErrPending
			}
			resume = true
		}
		for _, e := range p.Entries {
			if string(tx.Bucket(headsBucket).Get([]byte(e.Path))) != e.Expected {
				return ErrConflict
			}
			if tx.Bucket(versionsBucket).Get([]byte(e.Next.ID)) != nil {
				return ErrIdentity
			}
			if !resume {
				if err := c.store.RequireVacant(e.Next.ID); err != nil {
					return err
				}
			}
		}
		if err := reservePublication(tx, p); err != nil {
			return err
		}
		if resume {
			return nil
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return tx.Bucket(metaBucket).Put(intakeKey, raw)
	})
	if err != nil {
		return Record{}, err
	}
	if existing != nil {
		return clone(*existing), nil
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if err := receive(ctx, resume); err != nil {
		return Record{}, err
	}
	return c.commitLocked(ctx, epoch, p, publisher)
}
