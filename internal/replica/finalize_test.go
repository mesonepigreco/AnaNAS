package replica

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func completionFixture(t *testing.T) (*fixture, *index.DB, index.Upload) {
	t.Helper()
	f := setup(t)
	db, err := index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("uploaded snapshot")
	path := filepath.Join(f.root, "file")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, false); err != nil {
		t.Fatal(err)
	}
	p := f.proposal("uploaded", "file", "", data)
	p.Client = id
	u := index.Upload{Namespace: hash.SumBytes([]byte("remote namespace")).Hex(), Proposal: p, Generations: []uint64{1}, DeltaBytes: []int64{100}, DeltaDigests: []hash.Digest{hash.SumBytes([]byte("retained wire"))}}
	if err := f.store.ReceiveVerified(context.Background(), p.Entries[0].Next.ID, int64(len(data)), hash.SumBytes(data), func(w io.Writer) error { _, err := w.Write(data); return err }); err != nil {
		t.Fatal(err)
	}
	if err := db.PrepareUpload(u); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUploadCommit(u.Namespace, journal.Record{Proposal: p, Sequence: 7, Epoch: 4, Committed: true}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingUpload()
	if err != nil || pending == nil {
		t.Fatal(pending, err)
	}
	return f, db, *pending
}

func completionOptions() PushOptions {
	return PushOptions{Writes: true, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }}
}

func TestFinishUploadResumesBetweenDatabasesAndPreservesEdit(t *testing.T) {
	f, db, u := completionFixture(t)
	ctx := context.Background()
	// Commit the first database, then stop before the observer index transaction.
	if _, err := f.c.AdoptReplica(ctx, f.c.Epoch(), u.Namespace, *committedUpload(&u), func(ctx context.Context) error {
		_, err := retainedManifest(ctx, f.store, u.Proposal.Entries[0].Next, newPacer(ctx, 1<<30))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f.c.Close()
	var err error
	f.c, err = journal.Open(f.state, id("namespace"))
	if err != nil {
		t.Fatal(err)
	}
	db, err = index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	path := filepath.Join(f.root, "file")
	newer := []byte("newer edit must stay local")
	if err := os.WriteFile(path, newer, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, true); err != nil {
		t.Fatal(err)
	}
	if err := FinishUpload(ctx, db, f.c, f.store, u.Namespace, completionOptions()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, newer) {
		t.Fatal("completion replaced live content", err)
	}
	base, err := db.Base("file")
	if err != nil || base == nil || base.Content.ID != u.Proposal.Entries[0].Next.ID || base.Content.Blocks[0].Digest != hash.SumBytes([]byte("uploaded snapshot")) {
		t.Fatal("completion hashed live content", base, err)
	}
	if dirty, err := db.DirtyPage("", 1); err != nil || len(dirty) != 1 || dirty[0].Generation != 2 {
		t.Fatal("newer edit cleared", dirty, err)
	}
	if pending, err := db.PendingUpload(); err != nil || pending != nil {
		t.Fatal(pending, err)
	}
	if changes, err := f.c.Changes(0, 2); err != nil || len(changes) != 1 {
		t.Fatal("duplicate adoption", changes, err)
	}
	if err := FinishUpload(ctx, db, f.c, f.store, u.Namespace, completionOptions()); err != nil {
		t.Fatal(err)
	}
}

func TestFinishUploadRejectsCorruptSnapshotWithoutAcknowledgement(t *testing.T) {
	f, db, u := completionFixture(t)
	defer db.Close()
	path := filepath.Join(f.state, u.Proposal.Entries[0].Next.ID+".ready")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, int(u.Proposal.Entries[0].Next.Size)), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if err := FinishUpload(context.Background(), db, f.c, f.store, u.Namespace, completionOptions()); err == nil {
		t.Fatal("corrupt snapshot acknowledged")
	}
	if base, err := db.Base("file"); err != nil || base != nil {
		t.Fatal(base, err)
	}
	if head, pending, err := f.c.Head("file"); err != nil || pending || head != nil {
		t.Fatal("failed adoption changed journal", head, pending, err)
	}
	if pending, err := db.PendingUpload(); err != nil || pending == nil {
		t.Fatal("failed completion lost outbox", pending, err)
	}
}
