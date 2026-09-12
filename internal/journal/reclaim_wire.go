package journal

import (
	"context"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/delta"
)

// ReleaseUploadWire requires exact committed local adoption, which is permitted
// only after authenticating the remote receipt and verifying retained snapshots.
// The caller still retains that receipt/outbox until index finalization. U means
// wire allowance is charged; S means only snapshots/metadata remain charged.
// A crash during unlink/fsync retains U and retries; credit and S are atomic.
// No current or historical snapshot is removed, and entry count is unchanged.
func (c *Coordinator) ReleaseUploadWire(ctx context.Context, namespace string, p Proposal) error {
	p.Entries = append([]Entry(nil), p.Entries...)
	token, _, err := uploadCharge(p)
	if err != nil {
		return err
	}
	var credit int64
	for _, e := range p.Entries {
		if !e.Next.Directory && !e.Next.Tombstone {
			bound, err := delta.WireBound(e.Next.Size)
			if err != nil {
				return err
			}
			credit += bound
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("coordinator closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var budgeted, released bool
	check := func(tx *bolt.Tx) error {
		u, err := readBudget(tx)
		if err != nil || u == nil {
			return err
		}
		budgeted = true
		if err := checkUploadReservation(tx, p, namespace); err != nil {
			return err
		}
		if err := checkCommittedUpload(tx, p); err != nil {
			return err
		}
		if tx.Bucket(metaBucket).Get([]byte("pending")) != nil || tx.Bucket(metaBucket).Get(intakeKey) != nil {
			return ErrPending
		}
		released = tx.Bucket(reservationsBucket).Get([]byte(p.ID))[0] == 'S'
		if !released && u.ReservedBytes < credit {
			return fmt.Errorf("invalid wire release accounting")
		}
		return nil
	}
	if err := c.db.View(check); err != nil {
		return err
	}
	// Legacy explicit fixtures did not claim artifact ownership with a budget.
	if !budgeted {
		return nil
	}
	for _, e := range p.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if released || e.Next.Directory || e.Next.Tombstone {
			if err := c.store.RequireWireVacant(e.Next.ID); err != nil {
				return err
			}
		} else if err := c.store.RemoveCommittedWire(e.Next.ID); err != nil {
			return err
		}
	}
	if released {
		return nil
	}
	// One directory flush for the bounded batch, then one atomic ledger update.
	if err := c.store.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		if released {
			return ErrIdentity
		}
		u, err := readBudget(tx)
		if err != nil {
			return err
		}
		u.ReservedBytes -= credit
		if err := updateBudget(tx, u); err != nil {
			return err
		}
		token[0] = 'S'
		return tx.Bucket(reservationsBucket).Put([]byte(p.ID), token)
	})
}
