package journal

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/manifest"
)

var ackPoliciesBucket = []byte("ack-policies-v1")

// AcknowledgeRange binds a logical client to one exclusion policy and advances
// at most one page of committed batches. The authenticated client asserts durable
// application/bookkeeping; the NAS cannot inspect the PC's disk. This alone is
// not permission to reclaim versions needed by current bases or conflict pins.
// Identical retries perform no write. Changed policy, gaps and legacy unbound
// cursors require reconciliation, not retroactive attribution.
func (c *Coordinator) AcknowledgeRange(client, policy string, expected, through uint64) error {
	if !manifest.ValidID(client) || !manifest.ValidID(policy) || through <= expected || through-expected > MaxPage {
		return fmt.Errorf("bounded acknowledgement range required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("coordinator closed")
	}
	duplicate := false
	check := func(tx *bolt.Tx) error {
		if err := c.check(tx, c.epoch); err != nil {
			return err
		}
		current, err := number(tx.Bucket(cursorsBucket).Get([]byte(client)))
		if err != nil {
			return err
		}
		policies := tx.Bucket(ackPoliciesBucket)
		var bound []byte
		if policies != nil {
			bound = policies.Get([]byte(client))
		}
		if bound != nil && string(bound) != policy {
			return fmt.Errorf("acknowledgement policy changed")
		}
		if bound == nil && current != 0 {
			return fmt.Errorf("unbound legacy acknowledgement requires reconciliation")
		}
		if current == through && bound != nil {
			duplicate = true
			return nil
		}
		if current != expected {
			return ErrConflict
		}
		if bound == nil && policies != nil {
			count := 0
			cursor := policies.Cursor()
			for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
				count++
				if count >= 64 {
					return fmt.Errorf("acknowledgement client limit reached")
				}
			}
		}
		for seq := expected + 1; ; seq++ {
			id := tx.Bucket(changesBucket).Get(key(seq))
			if id == nil {
				return fmt.Errorf("cannot acknowledge missing committed batch")
			}
			r, err := decode(tx.Bucket(operationsBucket).Get(id))
			if err != nil {
				return err
			}
			if !r.Committed || r.Sequence != seq {
				return fmt.Errorf("cannot acknowledge uncommitted batch")
			}
			if seq == through {
				break
			}
		}
		return nil
	}
	if err := c.db.View(check); err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		b, err := tx.CreateBucketIfNotExists(ackPoliciesBucket)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(client), []byte(policy)); err != nil {
			return err
		}
		return tx.Bucket(cursorsBucket).Put([]byte(client), key(through))
	})
}
