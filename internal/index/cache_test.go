package index

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/journal"
)

func TestCachePinsKeepBasesConflictsIncomingAndBufferedChanges(t *testing.T) {
	d := openSyncTest(t)
	if err := d.Put([]Record{{Path: "file", Fingerprint: Fingerprint{Size: 1}}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Acknowledge("file", "", 1, version("a")); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordConflict("file", Conflict{ID: strings.Repeat("f", 64), BaseID: version("a").Content.ID, LocalID: version("b").Content.ID, RemoteID: version("c").Content.ID, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	e := journal.Entry{Path: "file", Expected: version("d").Content.ID, Next: journal.Version{ID: version("e").Content.ID, Size: 1}}
	if err := d.RecordIncoming(e, "retained conflict"); err != nil {
		t.Fatal(err)
	}
	if err := d.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(remoteInbox)
		if err != nil {
			return err
		}
		x := changefeed.Batch{Proposal: journal.Proposal{Entries: []journal.Entry{{Path: "elsewhere", Expected: strings.Repeat("1", 64), Next: journal.Version{ID: strings.Repeat("2", 64)}}}}}
		raw, _ := json.Marshal(x)
		return b.Put(remoteKey(1), raw)
	}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	finished := make(chan error, 1)
	err := d.WithCachePins(context.Background(), []journal.Entry{e}, func(pins map[string]bool) error {
		for _, s := range []string{"a", "b", "c", "d", "e", "1", "2"} {
			if !pins[strings.Repeat(s, 64)] {
				t.Fatalf("lost pinned %s", s)
			}
		}
		go func() { close(entered); finished <- d.RecordIncoming(e, "concurrent choice") }()
		<-entered
		select {
		case err := <-finished:
			t.Fatalf("reference changed during removal: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestCacheGuardPinsPendingUploadAndRefusesCorruptReference(t *testing.T) {
	d := openSyncTest(t)
	u := uploadFixture(t, d)
	if err := d.PrepareUpload(u); err != nil {
		t.Fatal(err)
	}
	if err := d.WithCachePins(context.Background(), nil, func(pins map[string]bool) error {
		if !pins[u.Proposal.Entries[0].Next.ID] {
			t.Fatal("outbox not pinned")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	other := openSyncTest(t)
	if err := other.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bases).Put([]byte("file"), []byte("corrupt manifest")) }); err != nil {
		t.Fatal(err)
	}
	if err := other.WithCachePins(context.Background(), []journal.Entry{{Path: "file"}}, func(map[string]bool) error { t.Fatal("corrupt pin accepted"); return nil }); err == nil {
		t.Fatal("corrupt base did not fail closed")
	}
}
