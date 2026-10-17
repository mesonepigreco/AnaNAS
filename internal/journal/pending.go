package journal

import bolt "go.etcd.io/bbolt"

func (c *Coordinator) PendingOperation() (bool, error) {
	var pending bool
	err := c.db.View(func(tx *bolt.Tx) error {
		m := tx.Bucket(metaBucket)
		pending = m.Get([]byte("pending")) != nil || m.Get(intakeKey) != nil
		return nil
	})
	return pending, err
}
