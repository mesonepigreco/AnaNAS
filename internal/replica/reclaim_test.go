package replica

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func TestWorkerResumesAfterSpoolReclaimBeforeOutboxFinalization(t *testing.T) {
	if dir := os.Getenv("ANANAS_RECLAIM_CRASH_TEST"); dir != "" {
		c, err := journal.Open(filepath.Join(dir, "state"), id("namespace"))
		if err != nil {
			t.Fatal(err)
		}
		db, err := index.Open(filepath.Join(dir, "state", "index.db"), filepath.Join(dir, "root"))
		if err != nil {
			t.Fatal(err)
		}
		u, err := db.PendingUpload()
		if err != nil || u == nil || u.Receipt == nil {
			t.Fatal(u, err)
		}
		if err := c.ReleaseUploadWire(context.Background(), u.Namespace, u.Proposal); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "wire-reclaimed")
		time.Sleep(time.Hour)
		t.Fatal("reclaim fixture was not killed")
	}
	f, db, root := preparationFixture(t)
	namespace := id("remote namespace")
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}, namespace); err != nil {
		t.Fatal(err)
	}
	observeFile(t, db, f.root, "file", []byte("uploaded snapshot"))
	u, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"file"}, namespace, completionOptions())
	if err != nil || u == nil {
		t.Fatal(u, err)
	}
	receipt := journal.Record{Proposal: u.Proposal, Sequence: 1, Epoch: 1, Committed: true}
	if err := db.RecordUploadCommit(namespace, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.AdoptReplica(context.Background(), f.c.Epoch(), namespace, receipt, func(ctx context.Context) error {
		_, err := retainedManifest(ctx, f.store, u.Proposal.Entries[0].Next, newPacer(ctx, 1<<30))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Stop between the journal's credit and the index's finalization, then
	// recover with the real worker completion path and no network implementation.
	db.Close()
	f.c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerResumesAfterSpoolReclaimBeforeOutboxFinalization$")
	cmd.Env = append(os.Environ(), "ANANAS_RECLAIM_CRASH_TEST="+filepath.Dir(f.state))
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "wire-reclaimed\n" {
		t.Fatal(line, err)
	}
	if other, err := journal.Open(f.state, id("namespace")); err == nil {
		other.Close()
		t.Fatal("live cleanup owner lock stolen")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal("cleanup fixture was not live", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("cleanup fixture finished before kill")
	}
	f.c, err = journal.Open(f.state, id("namespace"))
	if err != nil {
		t.Fatal(err)
	}
	db, err = index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	before, err := f.c.PublicationBudgetUsage()
	if err != nil || before == nil || before.ReservedBytes != (192<<10)+int64(len("uploaded snapshot")) {
		t.Fatal("killed cleanup did not persist wire credit", before, err)
	}
	observeFile(t, db, f.root, "file", []byte("newer local edit"))
	w, err := NewWorker(db, root, f.c, f.store, f.publisher, &quietWorkerRemote{}, WorkerOptions{Namespace: namespace, PushOptions: completionOptions(), Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.finishOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.PendingUpload(); err != nil || pending != nil {
		t.Fatal("receipt/outbox not finalized", pending, err)
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "file")); err != nil || string(data) != "newer local edit" {
		t.Fatal("newer content changed", err)
	}
	if err := f.store.RequireWireVacant(u.Proposal.Entries[0].Next.ID); err != nil {
		t.Fatal(err)
	}
	if after, err := f.c.PublicationBudgetUsage(); err != nil || after == nil || *after != *before {
		t.Fatal("restart credited twice", after, err)
	}
	if base, err := db.Base("file"); err != nil || base == nil || base.Content.ID != u.Proposal.Entries[0].Next.ID {
		t.Fatal("retained base lost", base, err)
	}
}
