package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

func TestDirectoryBaseReplayAndRestart(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "index.db")
	d, err := Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	id := hash.SumBytes([]byte("directory-version")).Hex()
	op := hash.SumBytes([]byte("directory-create")).Hex()
	client := hash.SumBytes([]byte("client")).Hex()
	entry := journal.Entry{Path: "folder", Next: journal.Version{ID: id, Directory: true}}
	page, err := changefeed.Filter(hash.SumBytes([]byte("namespace")).Hex(), nil, changefeed.Request{Limit: 1}, []journal.Record{{Proposal: journal.Proposal{ID: op, Client: client, Entries: []journal.Entry{entry}}, Sequence: 1, Epoch: 1, Committed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	if err := d.Put([]Record{{Path: "folder", Fingerprint: Fingerprint{Mode: uint32(os.ModeDir)}}}, false); err != nil {
		t.Fatal(err)
	}
	base := &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: id, Directory: true}}
	// A child edit can bump directory metadata while publication completes.
	if err := d.Put([]Record{{Path: "folder", Fingerprint: Fingerprint{Mode: uint32(os.ModeDir), MtimeNS: 1}}}, true); err != nil {
		t.Fatal(err)
	}
	if cleared, err := d.Acknowledge("folder", "", 1, base); err != nil || cleared {
		t.Fatal("newer directory generation lost", cleared, err)
	}
	d.Close()
	d, err = Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.Base("folder")
	if err != nil || !got.Content.Directory || got.Content.ID != id {
		t.Fatal(got, err)
	}
	if err := d.CompleteRemoteBatch(1, op); err != nil {
		t.Fatal(err)
	}
	if dirty, err := d.DirtyPage("", 1); err != nil || len(dirty) != 1 {
		t.Fatal("replay cleared newer child work", dirty, err)
	}
	// Identity reuse cannot turn an acknowledged directory into an empty file.
	fake := &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: id}}
	if _, err := d.Acknowledge("folder", id, 2, fake); err == nil {
		t.Fatal("directory identity reused as file")
	}
	deletion := journal.Entry{Path: "folder", Expected: id, Next: journal.Version{ID: hash.SumBytes([]byte("deleted")).Hex(), Tombstone: true}}
	nextOp := hash.SumBytes([]byte("directory-delete")).Hex()
	next, err := changefeed.Filter(page.Namespace, nil, changefeed.Request{Namespace: page.Namespace, Policy: page.Policy, After: 1, Limit: 1}, []journal.Record{{Proposal: journal.Proposal{ID: nextOp, Client: client, Entries: []journal.Entry{deletion}}, Sequence: 2, Epoch: 1, Committed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReceiveRemotePage(next); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteRemoteBatch(2, nextOp); !errors.Is(err, ErrStale) {
		t.Fatal("unapplied delete completed", err)
	}
	if err := d.Put([]Record{{Path: "folder", Missing: true}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Acknowledge("folder", id, 3, &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: deletion.Next.ID, Tombstone: true}}); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteRemoteBatch(2, nextOp); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyFileBaseCannotCompleteDirectoryBatch(t *testing.T) {
	d := openSyncTest(t)
	p := remotePage(t, false)
	p.Batches[0].Entries[0].Next = journal.Version{ID: version("a").Content.ID, Directory: true}
	if err := d.Put([]Record{{Path: "file"}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Acknowledge("file", "", 1, &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: version("a").Content.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := d.ReceiveRemotePage(p); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteRemoteBatch(1, p.Batches[0].ID); !errors.Is(err, ErrStale) {
		t.Fatal("empty file completed a directory", err)
	}
}
