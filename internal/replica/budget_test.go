package replica

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func TestPreparationBudgetRefusalLeavesOnlyAbortableMetadata(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	namespace := id("remote namespace")
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 192 << 10, MaxEntries: 1}, namespace); err != nil {
		t.Fatal(err)
	}
	observeFile(t, db, f.root, "file", []byte("keep local"))
	u, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"file"}, namespace, completionOptions())
	if !errors.Is(err, journal.ErrBudget) || u != nil {
		t.Fatal("over-budget snapshot accepted", u, err)
	}
	p, err := db.PendingUploadPreparation()
	if err != nil || p == nil {
		t.Fatal("lost quota-refused preparation", p, err)
	}
	if err := f.store.RequireUploadVacant(p.Sources[0].ID); err != nil {
		t.Fatal("quota refusal wrote content", err)
	}
	if err := AbortUploadPreparation(context.Background(), db, f.c, f.store, namespace); err != nil {
		t.Fatal(err)
	}
	if p, err := db.PendingUploadPreparation(); err != nil || p != nil {
		t.Fatal("quota refusal not recoverable", p, err)
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "file")); err != nil || string(data) != "keep local" {
		t.Fatal("quota refusal changed source", err)
	}
	uUsage, err := f.c.PublicationBudgetUsage()
	if err != nil || uUsage == nil || uUsage.ReservedBytes != 0 || uUsage.Entries != 0 {
		t.Fatal("refusal charged cache", uUsage, err)
	}
}

func TestPullBudgetPrecedesNetworkAndRetainsRetryCharge(t *testing.T) {
	for _, fits := range []bool{false, true} {
		f := setup(t)
		limit := int64(192 << 10)
		if fits {
			limit = 1 << 20
		}
		if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: limit, MaxEntries: 2}, id("remote namespace")); err != nil {
			t.Fatal(err)
		}
		p := f.proposal("budget-pull", "file", "", []byte("remote"))
		f.remote.fail = true
		_, err := f.puller.Pull(context.Background(), p)
		if !fits {
			if !errors.Is(err, journal.ErrBudget) || f.remote.calls != 0 {
				t.Fatal("budget refusal downloaded", f.remote.calls, err)
			}
			continue
		}
		if err == nil || f.remote.calls != 1 {
			t.Fatal("missing download failure", err)
		}
		u, err := f.c.PublicationBudgetUsage()
		if err != nil || u == nil || u.Entries != 1 {
			t.Fatal("interrupted pull not reserved", u, err)
		}
		f.remote.fail = false
		if _, err := f.puller.Pull(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		after, err := f.c.PublicationBudgetUsage()
		if err != nil || after == nil || *after != *u {
			t.Fatal("retry changed download charge", after, err)
		}
	}
}

func TestWorkerRequiresBudgetBeforeAnyNetworkWork(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	remote := &quietWorkerRemote{}
	if _, err := NewWorker(db, root, f.c, f.store, f.publisher, remote, WorkerOptions{Namespace: id("remote namespace"), PushOptions: completionOptions(), Ready: func() bool { return true }}); err == nil {
		t.Fatal("unbudgeted automatic worker accepted")
	}
	if remote.feeds.Load() != 0 {
		t.Fatal("unbudgeted constructor contacted NAS")
	}
}

func TestWorkerRefusesUnaccountedDirectoryOutboxAfterBootstrap(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	path := filepath.Join(f.root, "folder")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "folder", Fingerprint: content.Fingerprint(st)}}, true); err != nil {
		t.Fatal(err)
	}
	namespace := id("remote namespace")
	u, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"folder"}, namespace, completionOptions())
	if err != nil || u == nil {
		t.Fatal(u, err)
	}
	// A directory-only old outbox has no candidate files, so the private-state
	// bootstrap alone cannot prove that index work has a reservation.
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}, namespace); err != nil {
		t.Fatal(err)
	}
	remote := &quietWorkerRemote{}
	w, err := NewWorker(db, root, f.c, f.store, f.publisher, remote, WorkerOptions{Namespace: namespace, PushOptions: completionOptions(), Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.finishOutbox(context.Background()); err == nil {
		t.Fatal("unaccounted outbox sent")
	}
	if pending, err := db.PendingUpload(); err != nil || pending == nil || pending.Proposal.ID != u.Proposal.ID {
		t.Fatal("unaccounted outbox discarded", pending, err)
	}
}
