package index

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/journal"
)

var activityCounts = []byte("activity-counts-v1")
var activityRecent = []byte("activity-recent-v1")

const recentActivityLimit = 12

// RecentUpdate is one successfully finalized synchronized path. The compact
// activity ledger is local UI state, not synchronization protocol state.
type RecentUpdate struct {
	Path      string    `json:"path"`
	Direction string    `json:"direction"`
	Action    string    `json:"action"`
	At        time.Time `json:"at"`
}

type Activity struct {
	UpdatedLast24Hours uint64         `json:"updatedLast24Hours"`
	Recent             []RecentUpdate `json:"recentUpdates"`
}

func timeKey(second int64) []byte {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], uint64(second))
	return key[:]
}

func numberValue(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return encoded[:]
}

func recordActivity(tx *bolt.Tx, entries []journal.Entry, direction string, now time.Time) error {
	if direction != "to NAS" && direction != "from NAS" {
		return fmt.Errorf("invalid activity direction")
	}
	if len(entries) == 0 {
		return nil
	}
	now = now.UTC().Truncate(time.Second)
	counts := tx.Bucket(activityCounts)
	key := timeKey(now.Unix())
	var count uint64
	if raw := counts.Get(key); raw != nil {
		if len(raw) != 8 {
			return fmt.Errorf("invalid activity count")
		}
		count = binary.BigEndian.Uint64(raw)
	}
	if uint64(len(entries)) > ^uint64(0)-count {
		return fmt.Errorf("activity count overflow")
	}
	if err := counts.Put(key, numberValue(count+uint64(len(entries)))); err != nil {
		return err
	}
	cutoff := timeKey(now.Add(-24 * time.Hour).Unix())
	for cursor, k, _ := counts.Cursor(), []byte(nil), []byte(nil); ; {
		k, _ = cursor.First()
		if k == nil || string(k) >= string(cutoff) {
			break
		}
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	recent := tx.Bucket(activityRecent)
	for _, entry := range entries {
		action := "updated"
		if entry.Next.Tombstone {
			action = "deleted"
		} else if entry.Next.Directory {
			action = "folder"
		} else if entry.Expected == "" {
			action = "added"
		}
		sequence, err := recent.NextSequence()
		if err != nil {
			return err
		}
		raw, err := json.Marshal(RecentUpdate{Path: entry.Path, Direction: direction, Action: action, At: now})
		if err != nil {
			return err
		}
		if err := recent.Put(numberValue(sequence), raw); err != nil {
			return err
		}
	}
	cursor := recent.Cursor()
	recentCount := 0
	for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
		recentCount++
	}
	for k, _ := cursor.First(); k != nil && recentCount > recentActivityLimit; k, _ = cursor.Next() {
		if err := cursor.Delete(); err != nil {
			return err
		}
		recentCount--
	}
	return nil
}

// Activity returns an exact per-second rolling 24-hour count and a bounded,
// newest-first display list without enumerating synchronized files.
func (d *DB) Activity(now time.Time) (Activity, error) {
	var result Activity
	cutoff := timeKey(now.UTC().Add(-24 * time.Hour).Unix())
	err := d.db.View(func(tx *bolt.Tx) error {
		counts := tx.Bucket(activityCounts)
		cursor := counts.Cursor()
		for k, raw := cursor.Seek(cutoff); k != nil; k, raw = cursor.Next() {
			if len(raw) != 8 {
				return fmt.Errorf("invalid activity count")
			}
			value := binary.BigEndian.Uint64(raw)
			if value > ^uint64(0)-result.UpdatedLast24Hours {
				return fmt.Errorf("activity count overflow")
			}
			result.UpdatedLast24Hours += value
		}
		recent := tx.Bucket(activityRecent).Cursor()
		for k, raw := recent.Last(); k != nil && len(result.Recent) < recentActivityLimit; k, raw = recent.Prev() {
			var update RecentUpdate
			if err := json.Unmarshal(raw, &update); err != nil {
				return err
			}
			result.Recent = append(result.Recent, update)
		}
		return nil
	})
	return result, err
}
