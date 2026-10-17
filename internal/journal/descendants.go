package journal

import (
	"strings"

	bolt "go.etcd.io/bbolt"
)

// LiveDescendant checks retained heads, including children the native observer
// has not indexed. Missing observations must not allow a parent tombstone first.
func (c *Coordinator) LiveDescendant(path string) (bool, error) {
	if err := validatePath(path); err != nil {
		return false, err
	}
	live := false
	err := c.db.View(func(tx *bolt.Tx) error {
		prefix := path + "/"
		cursor := tx.Bucket(headsBucket).Cursor()
		for k, id := cursor.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, id = cursor.Next() {
			v, err := readVersion(tx, string(id))
			if err != nil {
				return err
			}
			if !v.Tombstone {
				live = true
				break
			}
		}
		return nil
	})
	return live, err
}
