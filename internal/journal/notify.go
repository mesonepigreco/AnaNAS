package journal

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// Observe returns a durable high-water sequence and the signal for subsequent
// commits atomically with respect to publication. Signals carry no paths or
// queued records. Slow readers simply obtain the latest sequence on their next
// observation. Close wakes all readers; their next Observe returns an error.
func (c *Coordinator) Observe() (uint64, <-chan struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, nil, fmt.Errorf("journal closed")
	}
	var sequence uint64
	err := c.db.View(func(tx *bolt.Tx) error {
		var err error
		sequence, err = number(tx.Bucket(metaBucket).Get([]byte("sequence")))
		return err
	})
	return sequence, c.changed, err
}

// Caller holds c.mu and has successfully committed the database transaction.
func (c *Coordinator) notifyCommitted() {
	close(c.changed)
	c.changed = make(chan struct{})
}
