package journal

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/manifest"
)

// BindReplicaOrigin pins a PC replica to its configured remote namespace before
// pulling content. Existing unbound operations require explicit reconciliation;
// a constructor must not silently assign their provenance to a new target.
func (c *Coordinator) BindReplicaOrigin(namespace string) error {
	if !manifest.ValidID(namespace) {
		return fmt.Errorf("replica origin required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("coordinator closed")
	}
	bound := false
	if err := c.db.View(func(tx *bolt.Tx) error {
		u, err := readBudget(tx)
		if err != nil {
			return err
		}
		if u != nil && u.ReplicaNamespace != namespace {
			return fmt.Errorf("publication-only budget cannot account for replica upload spools")
		}
		if err := c.check(tx, c.epoch); err != nil {
			return err
		}
		if prior := tx.Bucket(metaBucket).Get(replicaOriginKey); prior != nil {
			if string(prior) != namespace {
				return fmt.Errorf("replica origin requires reconciliation")
			}
			bound = true
		}
		return nil
	}); err != nil {
		return err
	}
	if bound {
		return nil
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		if err := c.check(tx, c.epoch); err != nil {
			return err
		}
		m := tx.Bucket(metaBucket)
		if prior := m.Get(replicaOriginKey); prior != nil {
			if string(prior) != namespace {
				return fmt.Errorf("replica origin requires reconciliation")
			}
			return nil
		}
		first, _ := tx.Bucket(operationsBucket).Cursor().First()
		if first != nil || m.Get(intakeKey) != nil {
			return fmt.Errorf("unbound replica history requires reconciliation")
		}
		return m.Put(replicaOriginKey, []byte(namespace))
	})
}
