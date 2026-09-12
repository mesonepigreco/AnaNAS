package index

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/journal"
)

func TestActivityIsDurableBoundedAndRolling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path, "/local")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	old := []journal.Entry{{Path: "expired", Next: journal.Version{Tombstone: true}}}
	if err := db.db.Update(func(tx *bolt.Tx) error { return recordActivity(tx, old, "to NAS", now.Add(-25*time.Hour)) }); err != nil {
		t.Fatal(err)
	}
	entries := make([]journal.Entry, 15)
	for i := range entries {
		entries[i] = journal.Entry{Path: fmt.Sprintf("SampleDocuments/file-%02d", i), Next: journal.Version{}, Expected: "prior"}
	}
	entries[14].Next.Tombstone = true
	if err := db.db.Update(func(tx *bolt.Tx) error { return recordActivity(tx, entries, "from NAS", now) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	activity, err := db.Activity(now)
	if err != nil {
		t.Fatal(err)
	}
	if activity.UpdatedLast24Hours != 15 || len(activity.Recent) != recentActivityLimit {
		t.Fatalf("unexpected activity: %+v", activity)
	}
	if activity.Recent[0].Path != "SampleDocuments/file-14" || activity.Recent[0].Action != "deleted" || activity.Recent[0].Direction != "from NAS" {
		t.Fatalf("newest activity differs: %+v", activity.Recent[0])
	}
}
