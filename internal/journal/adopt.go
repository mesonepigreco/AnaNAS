package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/manifest"
)

var replicaOriginKey = []byte("replica-origin-v1")

// AdoptReplica records an authenticated remote commit as this replica's logical
// base, after the caller verifies retained candidates. It never publishes or
// inspects the visible tree: a newer local edit may already differ from that base.
// This is for a PC replica, not for accepting uploads on a NAS coordinator.
// The caller must authenticate the receipt, apply exclusions/limits, and retain
// immutable content. Verification runs under ownership outside a DB transaction.
// Metadata is committed atomically, with no prepared visible-publication state.
func (c *Coordinator) AdoptReplica(ctx context.Context, epoch uint64, namespace string, receipt Record, verify func(context.Context) error) (Record, error) {
	if !manifest.ValidID(namespace) || !receipt.Committed || receipt.Sequence == 0 || receipt.Epoch == 0 || verify == nil {
		return Record{}, fmt.Errorf("authenticated replica commit and verifier required")
	}
	if err := validate(receipt.Proposal); err != nil {
		return Record{}, err
	}
	p := clone(receipt).Proposal
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	var existing *Record
	check := func(tx *bolt.Tx) error {
		if err := c.check(tx, epoch); err != nil {
			return err
		}
		m := tx.Bucket(metaBucket)
		if err := checkUploadReservation(tx, p, namespace); err != nil {
			return err
		}
		if origin := m.Get(replicaOriginKey); origin != nil && string(origin) != namespace {
			return fmt.Errorf("replica origin requires reconciliation")
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
		if m.Get([]byte("pending")) != nil || m.Get(intakeKey) != nil {
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
		return nil
	}
	if err := c.db.View(check); err != nil {
		return Record{}, err
	}
	if existing != nil {
		// Older local publication may already contain this exact operation.
		// Still persist the authenticated origin binding on this path.
		err := c.db.Update(func(tx *bolt.Tx) error {
			if err := check(tx); err != nil {
				return err
			}
			return tx.Bucket(metaBucket).Put(replicaOriginKey, []byte(namespace))
		})
		return clone(*existing), err
	}
	if err := verify(ctx); err != nil {
		return Record{}, err
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	var result Record
	err := c.db.Update(func(tx *bolt.Tx) error {
		if err := check(tx); err != nil {
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
		result = Record{Proposal: p, Sequence: seq + 1, Epoch: epoch, Committed: true}
		for _, e := range p.Entries {
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
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := tx.Bucket(operationsBucket).Put([]byte(p.ID), raw); err != nil {
			return err
		}
		if err := tx.Bucket(changesBucket).Put(key(result.Sequence), []byte(p.ID)); err != nil {
			return err
		}
		if err := m.Put(replicaOriginKey, []byte(namespace)); err != nil {
			return err
		}
		return m.Put([]byte("sequence"), key(result.Sequence))
	})
	if err != nil {
		return Record{}, err
	}
	c.notifyCommitted()
	return clone(result), nil
}
