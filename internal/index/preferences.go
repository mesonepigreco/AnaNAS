package index

import bolt "go.etcd.io/bbolt"

func (d *DB) NotifySyncProblems(fallback bool) bool {
	value := fallback
	d.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(meta).Get([]byte("notify-sync-problems"))
		if raw != nil {
			value = string(raw) == "true"
		}
		return nil
	})
	return value
}
func (d *DB) SetNotifySyncProblems(value bool) error {
	text := "false"
	if value {
		text = "true"
	}
	return d.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(meta).Put([]byte("notify-sync-problems"), []byte(text)) })
}
