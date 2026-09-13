package replica

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/content"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func downloadInbox(t *testing.T, f *fixture, excluded bool) (*index.DB, journal.Proposal) {
	t.Helper()
	db, err := index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	p := f.proposal("download", "file", "", []byte("downloaded bytes"))
	var patterns []string
	if excluded {
		patterns = []string{"file"}
	}
	page, err := changefeed.Filter(id("remote namespace"), nil, changefeed.Request{Limit: 1, Exclusions: patterns}, []journal.Record{{Proposal: p, Sequence: 1, Epoch: 1, Committed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	return db, p
}

func TestFinishDownloadAfterRestartPreservesObservedEdit(t *testing.T) {
	f := setup(t)
	db, p := downloadInbox(t, f, false)
	ctx := context.Background()
	if err := FinishDownload(ctx, db, f.c, f.store, id("remote namespace"), completionOptions()); err == nil {
		t.Fatal("inbox receipt treated as local publication")
	}
	if _, err := f.puller.Pull(ctx, p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, "file")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, false); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f.c.Close()
	f.c, err = journal.Open(f.state, id("namespace"))
	if err != nil {
		t.Fatal(err)
	}
	db, err = index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	local := []byte("local edit after download publication")
	if err := os.WriteFile(path, local, 0600); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, true); err != nil {
		t.Fatal(err)
	}
	before, ok, err := db.Get("file")
	if err != nil || !ok {
		t.Fatal(before, err)
	}
	if err := FinishDownload(ctx, db, f.c, f.store, id("remote namespace"), completionOptions()); err != nil {
		t.Fatal(err)
	}
	after, ok, err := db.Get("file")
	if err != nil || !ok || after != before || !after.Dirty {
		t.Fatal("download completion changed observer state", before, after, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, local) {
		t.Fatal("live edit overwritten", err)
	}
	base, err := db.Base("file")
	if err != nil || base == nil || base.Content.ID != p.Entries[0].Next.ID || base.Content.Blocks[0].Digest != hash.SumBytes([]byte("downloaded bytes")) {
		t.Fatal("base used newer live content", base, err)
	}
	state, err := db.RemoteState()
	if err != nil || state.Completed != 1 || state.Received != 1 {
		t.Fatal(state, err)
	}
	if err := FinishDownload(ctx, db, f.c, f.store, id("remote namespace"), completionOptions()); err != nil {
		t.Fatal(err)
	}
}

func TestFinishDownloadCorruptCandidateKeepsInbox(t *testing.T) {
	f := setup(t)
	db, p := downloadInbox(t, f, false)
	defer db.Close()
	if _, err := f.puller.Pull(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.state, p.Entries[0].Next.ID+".ready")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, int(p.Entries[0].Next.Size)), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if err := FinishDownload(context.Background(), db, f.c, f.store, id("remote namespace"), completionOptions()); err == nil {
		t.Fatal("corrupt candidate completed")
	}
	if s, err := db.RemoteState(); err != nil || s.Completed != 0 {
		t.Fatal("corrupt completion advanced cursor", s, err)
	}
	if base, err := db.Base("file"); err != nil || base != nil {
		t.Fatal(base, err)
	}
}

func TestFinishDownloadAllExcludedNeedsNoPublication(t *testing.T) {
	f := setup(t)
	db, _ := downloadInbox(t, f, true)
	defer db.Close()
	if err := FinishDownload(context.Background(), db, f.c, f.store, id("remote namespace"), completionOptions()); err != nil {
		t.Fatal(err)
	}
	if s, err := db.RemoteState(); err != nil || s.Completed != 1 || s.ExcludedEntries != 1 {
		t.Fatal(s, err)
	}
	if f.remote.calls != 0 {
		t.Fatal("excluded entry downloaded")
	}
	if base, err := db.Base("file"); err != nil || base != nil {
		t.Fatal("excluded base materialized", base, err)
	}
}

func TestAlreadyDeletedFileDoesNotBlockLaterRemoteUpdates(t *testing.T) {
	f := setup(t)
	db, first := downloadInbox(t, f, false)
	defer db.Close()
	ctx := context.Background()
	if _, err := f.puller.Pull(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := FinishDownload(ctx, db, f.c, f.store, id("remote namespace"), completionOptions()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.root, "file")); err != nil {
		t.Fatal(err)
	}
	deleted := journal.Proposal{ID: id("simultaneous-delete"), Client: first.Client, Entries: []journal.Entry{{Path: "file", Expected: first.Entries[0].Next.ID, Next: journal.Version{ID: id("tombstone"), Tombstone: true}}}}
	next := f.proposal("after-deletion", "next-file", "", []byte("new content after deletion"))
	priorState, err := db.RemoteState()
	if err != nil {
		t.Fatal(err)
	}
	page, err := changefeed.Filter(id("remote namespace"), nil, changefeed.Request{Namespace: priorState.Namespace, Policy: priorState.Policy, After: 1, Limit: 2}, []journal.Record{{Proposal: deleted, Sequence: 2, Epoch: 1, Committed: true}, {Proposal: next, Sequence: 3, Epoch: 1, Committed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	for _, p := range []journal.Proposal{deleted, next} {
		if _, err := f.puller.Pull(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := FinishDownload(ctx, db, f.c, f.store, id("remote namespace"), completionOptions()); err != nil {
			t.Fatal(err)
		}
	}
	state, err := db.RemoteState()
	if err != nil || state.Completed != 3 {
		t.Fatal(state, err)
	}
	if f.remote.calls != 2 {
		t.Fatal("deletion performed unexpected download", f.remote.calls)
	}
	data, err := os.ReadFile(filepath.Join(f.root, "next-file"))
	if err != nil || string(data) != "new content after deletion" {
		t.Fatal(string(data), err)
	}
}

func TestFinishDownloadDirectoryAndDeletion(t *testing.T) {
	f := setup(t)
	db, err := index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	folder := journal.Proposal{ID: id("mkdir"), Client: id("remote-client"), Entries: []journal.Entry{{Path: "folder", Next: journal.Version{ID: id("folder"), Directory: true}}}}
	deleted := journal.Proposal{ID: id("rmdir"), Client: folder.Client, Entries: []journal.Entry{{Path: "folder", Expected: id("folder"), Next: journal.Version{ID: id("deleted folder"), Tombstone: true}}}}
	page, err := changefeed.Filter(id("remote namespace"), nil, changefeed.Request{Limit: 2}, []journal.Record{{Proposal: folder, Sequence: 1, Epoch: 1, Committed: true}, {Proposal: deleted, Sequence: 2, Epoch: 1, Committed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	for i, p := range []journal.Proposal{folder, deleted} {
		if _, err := f.puller.Pull(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if err := FinishDownload(context.Background(), db, f.c, f.store, id("remote namespace"), completionOptions()); err != nil {
			t.Fatal(err)
		}
		base, err := db.Base("folder")
		if err != nil || base == nil || base.Content.Directory != (i == 0) || base.Content.Tombstone != (i == 1) {
			t.Fatal(base, err)
		}
	}
	if s, err := db.RemoteState(); err != nil || s.Completed != 2 {
		t.Fatal(s, err)
	}
	if f.remote.calls != 0 {
		t.Fatal("directory metadata requested content")
	}
	if _, err := os.Stat(filepath.Join(f.root, "folder")); !os.IsNotExist(err) {
		t.Fatal("directory not removed", err)
	}
}
