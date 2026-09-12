package index

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/journal"
)

// RecordRemoteAcknowledged saves an exact confirmed NAS reply. Received batches
// alone cannot be acknowledged: through must be within the durable Completed
// prefix. A lost reply leaves the old value for an idempotent retry after restart.
func (d *DB) RecordRemoteAcknowledged(namespace, policy string, expected, through uint64) error {
	if through <= expected || through-expected > journal.MaxPage {
		return fmt.Errorf("bounded acknowledgement range required")
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		s, err := readRemoteState(tx)
		if err != nil {
			return err
		}
		if s.Namespace != namespace || s.Policy != policy || through > s.Completed {
			return ErrStale
		}
		if s.Acknowledged == through {
			return nil
		}
		if s.Acknowledged != expected {
			return ErrStale
		}
		s.Acknowledged = through
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return tx.Bucket(meta).Put(remoteStateKey, raw)
	})
}
