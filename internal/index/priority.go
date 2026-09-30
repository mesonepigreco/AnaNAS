package index

import (
	"encoding/json"
	"errors"
	"strings"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/changefeed"
)

// Metadata lookahead is bounded; no file payload enters the inbox.
const MaxRemoteBacklog = 100000
const MaxRemoteMetadataBytes = 64 << 20

var ErrRemoteBacklogFull = errors.New("remote metadata lookahead is full")

var remoteActiveKey = []byte("remote-active-v1")
var remotePriorityKey = []byte("remote-priority-directory-v1")
var remoteDone = []byte("remote-completed-gaps-v1")

func (d *DB) PrioritizeDirectory(path string) error {
	if path != "" {
		if err := syncPath(path); err != nil {
			return err
		}
	}
	return d.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(meta).Put(remotePriorityKey, []byte(path)) })
}

func (d *DB) PriorityDirectory() (string, error) {
	var path string
	err := d.db.View(func(tx *bolt.Tx) error { path = string(tx.Bucket(meta).Get(remotePriorityKey)); return nil })
	return path, err
}

func within(path, directory string) bool {
	return directory == "" || path == directory || strings.HasPrefix(path, directory+"/")
}
func related(a, b string) bool { return within(a, b) || within(b, a) }

// Pin a chosen operation until durable completion. A new UI request must never
// redirect finalization of the currently downloading file/batch, even at restart.
func selectRemote(tx *bolt.Tx, state RemoteState) error {
	if state.Received == state.Completed || tx.Bucket(meta).Get(remoteActiveKey) != nil {
		return nil
	}
	b := tx.Bucket(remoteInbox)
	if b == nil {
		return nil
	} // nextRemote reports the corrupt inbox.
	priority := string(tx.Bucket(meta).Get(remotePriorityKey))
	chosen := state.Completed + 1
	if priority != "" {
		cursor := b.Cursor()
		var candidate *changefeed.Batch
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var batch changefeed.Batch
			if err := json.Unmarshal(v, &batch); err != nil {
				return err
			}
			for _, e := range batch.Entries {
				if within(e.Path, priority) {
					candidate = &batch
					break
				}
			}
			if candidate != nil {
				break
			}
		}
		if candidate != nil {
			// Never pass an earlier operation touching this path or one of its parents
			// or children. Multi-path batches remain atomic and retain their dependencies.
			for k, v := cursor.Prev(); k != nil; k, v = cursor.Prev() {
				var earlier changefeed.Batch
				if err := json.Unmarshal(v, &earlier); err != nil {
					return err
				}
				dependency := false
				for _, a := range earlier.Entries {
					for _, b := range candidate.Entries {
						if related(a.Path, b.Path) {
							dependency = true
						}
					}
				}
				if dependency {
					candidate = &earlier
				}
			}
			chosen = candidate.Sequence
		}
	}
	return tx.Bucket(meta).Put(remoteActiveKey, remoteKey(chosen))
}
