package index

import (
	"errors"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
)

func remotePage(t *testing.T, excluded bool) changefeed.Page {
	t.Helper()
	entry := journal.Entry{Path: "file", Next: journal.Version{ID: version("a").Content.ID, Size: 1, Digest: hash.SumBytes([]byte("a"))}}
	r := journal.Record{Proposal: journal.Proposal{ID: hash.SumBytes([]byte("operation")).Hex(), Client: hash.SumBytes([]byte("remote-client")).Hex(), Entries: []journal.Entry{entry}}, Sequence: 1, Epoch: 1, Committed: true}
	var patterns []string
	if excluded {
		patterns = []string{"file"}
	}
	p, err := changefeed.Filter(hash.SumBytes([]byte("namespace")).Hex(), nil, changefeed.Request{Limit: 1, Exclusions: patterns}, []journal.Record{r})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRemoteInboxSurvivesRestartAndRequiresDurableCompletion(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "index.db")
	d, err := Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	p := remotePage(t, false)
	if err := d.Put([]Record{{Path: "file", Fingerprint: Fingerprint{Size: 1}}}, false); err != nil {
		t.Fatal(err)
	}
	if err := d.ReceiveRemotePage(p); err != nil {
		t.Fatal(err)
	}
	if err := d.ReceiveRemotePage(p); err != nil {
		t.Fatal("retry failed", err)
	}
	if err := d.CompleteRemoteBatch(1, p.Batches[0].ID); !errors.Is(err, ErrStale) {
		t.Fatal("receipt treated as sync", err)
	}
	d.Close()
	d, err = Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := d.RemoteState()
	if err != nil || s.Received != 1 || s.Completed != 0 {
		t.Fatal(s, err)
	}
	next, err := d.NextRemoteBatch()
	if err != nil || next == nil || next.ID != p.Batches[0].ID {
		t.Fatal(next, err)
	}
	empty := changefeed.Page{Namespace: p.Namespace, Policy: p.Policy, After: 1, Through: 1}
	if err := d.ReceiveRemotePage(empty); !errors.Is(err, ErrStale) {
		t.Fatal("queued a second page", err)
	}
	dirty, err := d.DirtyPage("", 1)
	if err != nil || len(dirty) != 1 {
		t.Fatal("receiving erased local dirty state", err)
	}
	if _, err := d.Acknowledge("file", "", 1, version("a")); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteRemoteBatch(1, p.Batches[0].ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := d.NextRemoteBatch(); err != nil || pending != nil {
		t.Fatal(pending, err)
	}
	if err := d.ReceiveRemotePage(empty); err != nil {
		t.Fatal(err)
	}
	empty.Policy = hash.SumBytes([]byte("changed exclusions")).Hex()
	if err := d.ReceiveRemotePage(empty); err == nil {
		t.Fatal("policy changed silently")
	}
}

func TestRemoteExcludedAndConflictDispositions(t *testing.T) {
	t.Run("excluded", func(t *testing.T) {
		d := openSyncTest(t)
		p := remotePage(t, true)
		if err := d.ReceiveRemotePage(p); err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteRemoteBatch(1, p.Batches[0].ID); err != nil {
			t.Fatal(err)
		}
		s, err := d.RemoteState()
		if err != nil || s.Completed != 1 || s.ExcludedEntries != 1 {
			t.Fatal(s, err)
		}
	})
	t.Run("conflict", func(t *testing.T) {
		d := openSyncTest(t)
		p := remotePage(t, false)
		if err := d.Put([]Record{{Path: "file", Fingerprint: Fingerprint{Size: 1}}}, false); err != nil {
			t.Fatal(err)
		}
		if err := d.ReceiveRemotePage(p); err != nil {
			t.Fatal(err)
		}
		conflict := Conflict{ID: hash.SumBytes([]byte("conflict")).Hex(), LocalID: version("b").Content.ID, RemoteID: version("a").Content.ID, Generation: 1}
		if err := d.RecordConflict("file", conflict); err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteRemoteBatch(1, p.Batches[0].ID); err != nil {
			t.Fatal(err)
		}
		state, err := d.PathState("file")
		if err != nil || state.Conflict == nil || *state.Conflict != conflict {
			t.Fatal("completion lost conflict", state, err)
		}
	})
}

func TestRemoteInboxMissingBatchFailsClosed(t *testing.T) {
	d := openSyncTest(t)
	p := remotePage(t, false)
	if err := d.ReceiveRemotePage(p); err != nil {
		t.Fatal(err)
	}
	if err := d.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(remoteInbox).Delete(remoteKey(1)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NextRemoteBatch(); err == nil {
		t.Fatal("missing record interpreted as up to date")
	}
	if err := d.CompleteRemoteBatch(1, p.Batches[0].ID); err == nil {
		t.Fatal("completed missing record")
	}
}
