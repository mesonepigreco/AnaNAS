package replica

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/journal"
)

func TestAutomaticCacheCleanupKeepsDownloadDeltaAndDeleteWorking(t *testing.T) {
	f, db, _ := preparationFixture(t)
	defer db.Close()
	ns := id("remote namespace")
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 10 << 20, MaxEntries: 100}, ns); err != nil {
		t.Fatal(err)
	}
	a := f.proposal("cache-first", "file", "", bytes.Repeat([]byte("a"), 65536))
	if _, err := f.puller.Pull(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b := f.proposal("cache-second", "file", a.Entries[0].Next.ID, bytes.Repeat([]byte("b"), 65536))
	if _, err := f.puller.Pull(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	w := &Worker{db: db, c: f.c, opts: WorkerOptions{Namespace: ns, CacheMaintenance: true}}
	if err := w.maintainCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.state, a.Entries[0].Next.ID+".ready")); !os.IsNotExist(err) {
		t.Fatal("automatic maintenance did not remove old snapshot", err)
	}
	data := append([]byte{'c'}, bytes.Repeat([]byte("b"), 65536)...)
	c := f.proposal("cache-third", "file", b.Entries[0].Next.ID, data)
	r, err := f.puller.Pull(context.Background(), c)
	if err != nil || r.Transfers[0].LiteralBytes != 1 || r.Transfers[0].ReusedBytes != 65536 {
		t.Fatal("download delta regressed", r, err)
	}
	d := f.proposal("cache-delete", "file", c.Entries[0].Next.ID, nil)
	d.Entries[0].Next.Tombstone = true
	d.Entries[0].Next.Digest = [32]byte{}
	if _, err := f.puller.Pull(context.Background(), d); err != nil {
		t.Fatal("deletion after maintenance", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "file")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestCacheCleanupPreservesDeltaBaseAndPendingOutbox(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	ns := id("remote namespace")
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 10 << 20, MaxEntries: 100}, ns); err != nil {
		t.Fatal(err)
	}
	remote := &comparisonRemote{}
	var old string
	for i, text := range []string{"first content", "second content", "third content"} {
		observeFile(t, db, f.root, "file", []byte(text))
		u, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, remote, []string{"file"}, ns, completionOptions())
		if err != nil || u == nil {
			t.Fatal(u, err)
		}
		if i == 0 {
			old = u.Proposal.Entries[0].Next.ID
		}
		if i == 2 {
			if r, err := MaintainCache(context.Background(), db, f.c, ns, "", 4096, true); err != nil || r.Removed != 1 {
				t.Fatal("unrelated old snapshot not reclaimed", r, err)
			}
			for _, id := range []string{u.Proposal.Entries[0].Expected, u.Proposal.Entries[0].Next.ID} {
				if _, err := os.Stat(filepath.Join(f.state, id+".ready")); err != nil {
					t.Fatal("outbox dependency removed", err)
				}
			}
			if _, err := os.Stat(filepath.Join(f.state, old+".ready")); !os.IsNotExist(err) {
				t.Fatal("obsolete snapshot retained", err)
			}
		}
		if err := db.RecordUploadCommit(ns, journal.Record{Proposal: u.Proposal, Sequence: uint64(i + 1), Epoch: 1, Committed: true}); err != nil {
			t.Fatal(err)
		}
		if err := FinishUpload(context.Background(), db, f.c, f.store, ns, completionOptions()); err != nil {
			t.Fatal(err)
		}
		v := u.Proposal.Entries[0].Next
		remote.v = &v
	}
	r, err := MaintainCache(context.Background(), db, f.c, ns, "", 4096, true)
	if err != nil || r.Removed != 1 {
		t.Fatal(r, err)
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "file")); err != nil || string(data) != "third content" {
		t.Fatal(string(data), err)
	}
	observeFile(t, db, f.root, "file", []byte("fourth content"))
	u, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, remote, []string{"file"}, ns, completionOptions())
	if err != nil || u == nil {
		t.Fatal("delta preparation after cleanup", u, err)
	}
}
