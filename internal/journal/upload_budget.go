package journal

import (
	"encoding/json"
	"fmt"
	"reflect"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
)

// Upload reservations bind operation/client/path/base/version/type/size. Content
// digests are verified later by snapshot creation and authenticated adoption;
// comparing blocks does not supply the whole-file digest before that snapshot.
func uploadCharge(p Proposal) ([]byte, int64, error) {
	if err := validate(p); err != nil {
		return nil, 0, err
	}
	p.Entries = append([]Entry(nil), p.Entries...)
	charge := budgetOperationOverhead
	var total int64
	for i := range p.Entries {
		v := &p.Entries[i].Next
		if v.Size > (8<<30)-total {
			return nil, 0, fmt.Errorf("upload cache reservation exceeds batch size")
		}
		total += v.Size
		charge += budgetEntryOverhead
		if !v.Directory && !v.Tombstone {
			v.Digest = hash.SumBytes(nil)
			// Snapshot plus the encoder's existing maximum spool length.
			wireBytes, err := delta.WireBound(v.Size)
			if err != nil {
				return nil, 0, err
			}
			charge += v.Size + wireBytes
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, 0, err
	}
	digest := hash.SumBytes(raw)
	// Publication tokens are exactly 32 bytes. A different purpose must never
	// reuse one operation's reservation, even when its proposal is identical.
	token := append([]byte{'U'}, digest[:]...)
	// Bind the charged amount as well: a future sizing-rule change must refuse
	// an old token until migrated, rather than credit a different amount.
	return append(token, key(uint64(charge))...), charge, nil
}

func updateBudget(tx *bolt.Tx, u *BudgetUsage) error {
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return tx.Bucket(metaBucket).Put(budgetKey, raw)
}

// ReserveUploadCache is called after durable local preparation and before any
// snapshot/wire write. Failure leaves that preparation available for abort; a
// crash cannot orphan a charged reservation outside its recovery record. The
// caller holds the index preparation lock and serializes replica operations.
func (c *Coordinator) ReserveUploadCache(p Proposal) error {
	token, charge, err := uploadCharge(p)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("coordinator closed")
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		u, err := readBudget(tx)
		if err != nil || u == nil {
			return err
		}
		if u.ReplicaNamespace == "" {
			return fmt.Errorf("replica cache budget required")
		}
		if err := unusedUpload(tx, p); err != nil {
			return err
		}
		b := tx.Bucket(reservationsBucket)
		if prior := b.Get([]byte(p.ID)); prior != nil {
			if string(prior) != string(token) {
				return ErrIdentity
			}
			return nil
		}
		for _, e := range p.Entries {
			if err := c.store.RequireUploadVacant(e.Next.ID); err != nil {
				return err
			}
			if string(tx.Bucket(headsBucket).Get([]byte(e.Path))) != e.Expected {
				return ErrConflict
			}
		}
		if charge > u.MaxBytes-u.ReservedBytes || int64(len(p.Entries)) > u.MaxEntries-u.Entries {
			return ErrBudget
		}
		u.ReservedBytes += charge
		u.Entries += int64(len(p.Entries))
		if err := updateBudget(tx, u); err != nil {
			return err
		}
		return b.Put([]byte(p.ID), token)
	})
}

func unusedUpload(tx *bolt.Tx, p Proposal) error {
	if tx.Bucket(metaBucket).Get([]byte("pending")) != nil || tx.Bucket(metaBucket).Get(intakeKey) != nil {
		return ErrPending
	}
	if tx.Bucket(operationsBucket).Get([]byte(p.ID)) != nil {
		return ErrIdentity
	}
	for _, e := range p.Entries {
		if tx.Bucket(versionsBucket).Get([]byte(e.Next.ID)) != nil {
			return ErrIdentity
		}
	}
	return nil
}

func checkUploadReservation(tx *bolt.Tx, p Proposal, namespace string) error {
	u, err := readBudget(tx)
	if err != nil || u == nil {
		return err
	}
	if u.ReplicaNamespace != namespace {
		return fmt.Errorf("matching replica cache budget required")
	}
	token, _, err := uploadCharge(p)
	if err != nil {
		return err
	}
	prior := tx.Bucket(reservationsBucket).Get([]byte(p.ID))
	if string(prior) == string(token) {
		return nil
	}
	token[0] = 'S'
	if string(prior) == string(token) {
		return checkCommittedUpload(tx, p)
	}
	return fmt.Errorf("upload cache reservation missing or different")
}

func checkCommittedUpload(tx *bolt.Tx, p Proposal) error {
	r, err := decode(tx.Bucket(operationsBucket).Get([]byte(p.ID)))
	if err != nil {
		return err
	}
	if !r.Committed || !reflect.DeepEqual(r.Proposal, p) {
		return ErrIdentity
	}
	for _, e := range p.Entries {
		v, err := readVersion(tx, e.Next.ID)
		if err != nil {
			return err
		}
		if v.Path != e.Path || v.Version != e.Next {
			return ErrIdentity
		}
	}
	return nil
}

// CheckUploadCache prevents an automatic dispatcher sending an old/unaccounted
// outbox after budget bootstrap. It never creates a reservation retroactively.
func (c *Coordinator) CheckUploadCache(p Proposal, namespace string) error {
	return c.checkDispatchCache(p, namespace, false)
}

// CheckUploadCompletion also accepts an already reclaimed wire reservation,
// backed by the exact committed local operation. Caller has a durable remote
// receipt and will finish locally, without opening/sending a wire spool again.
func (c *Coordinator) CheckUploadCompletion(p Proposal, namespace string) error {
	return c.checkDispatchCache(p, namespace, true)
}

func (c *Coordinator) checkDispatchCache(p Proposal, namespace string, completion bool) error {
	if err := validate(p); err != nil {
		return err
	}
	return c.db.View(func(tx *bolt.Tx) error {
		u, err := readBudget(tx)
		if err != nil {
			return err
		}
		if u == nil {
			return fmt.Errorf("replica cache budget required")
		}
		if err := checkUploadReservation(tx, p, namespace); err != nil {
			return err
		}
		if !completion && tx.Bucket(reservationsBucket).Get([]byte(p.ID))[0] != 'U' {
			return fmt.Errorf("upload spool already reclaimed; durable receipt required")
		}
		return nil
	})
}

// AbortUploadCache runs only for an untransmitted index preparation, never an
// outbox. It retains the charge until cleanup has removed/flushed exact owned
// candidates. A retry after release is permitted only when all four names remain
// absent. The callback cannot reenter journal mutations. Committed/prepared or
// differently reserved identities cannot be credited or removed.
func (c *Coordinator) AbortUploadCache(p Proposal, cleanup func() error) error {
	token, charge, err := uploadCharge(p)
	if err != nil {
		return err
	}
	if cleanup == nil {
		return fmt.Errorf("scoped cleanup required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("coordinator closed")
	}
	var budgeted, reserved bool
	if err := c.db.View(func(tx *bolt.Tx) error {
		if err := unusedUpload(tx, p); err != nil {
			return err
		}
		u, err := readBudget(tx)
		if err != nil || u == nil {
			return err
		}
		budgeted = true
		if u.ReplicaNamespace == "" {
			return fmt.Errorf("replica cache budget required")
		}
		prior := tx.Bucket(reservationsBucket).Get([]byte(p.ID))
		reserved = prior != nil
		if reserved && string(prior) != string(token) {
			return ErrIdentity
		}
		return nil
	}); err != nil {
		return err
	}
	if budgeted && !reserved {
		for _, e := range p.Entries {
			if err := c.store.RequireUploadVacant(e.Next.ID); err != nil {
				return err
			}
		}
		// Includes retry after the charge was released but before the index
		// forgot preparation. No unaccounted content can be silently removed.
		return nil
	}
	if err := cleanup(); err != nil {
		return err
	}
	if !budgeted {
		return nil
	}
	for _, e := range p.Entries {
		if err := c.store.RequireUploadVacant(e.Next.ID); err != nil {
			return err
		}
	}
	if err := c.store.Sync(); err != nil {
		return err
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		u, err := readBudget(tx)
		if err != nil {
			return err
		}
		if u == nil || u.ReservedBytes < charge || u.Entries < int64(len(p.Entries)) {
			return fmt.Errorf("invalid upload release accounting")
		}
		if err := unusedUpload(tx, p); err != nil {
			return err
		}
		if string(tx.Bucket(reservationsBucket).Get([]byte(p.ID))) != string(token) {
			return ErrIdentity
		}
		u.ReservedBytes -= charge
		u.Entries -= int64(len(p.Entries))
		if err := updateBudget(tx, u); err != nil {
			return err
		}
		return tx.Bucket(reservationsBucket).Delete([]byte(p.ID))
	})
}
