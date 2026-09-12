package transferapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/diff"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

func TestTLSUploadFromSnapshotAndDurableWire(t *testing.T) {
	dir := t.TempDir()
	root, state := filepath.Join(dir, "local"), filepath.Join(dir, "state")
	for _, path := range []string{root, state} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := index.Open(filepath.Join(state, "index.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clientID, err := db.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	f := setupWithClientID(t, true, nil, clientID)
	client := newTestClient(t, clientOptions(f))
	store, err := stage.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local, err := journal.Open(state, identifier("snapshot-local-replica"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	source, err := content.OpenRoot(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	base := make([]byte, 256*1024)
	if _, err := rand.Read(base); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pusher, err := replica.NewPusher(db, store, client, replica.PushOptions{Writes: true, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	previous := journal.Version{}
	var after uint64
	var feedNamespace, policy string
	for attempt, data := range [][]byte{base, append([]byte{9}, base...), nil} {
		path := filepath.Join(root, "file")
		r := index.Record{Path: "file", Missing: data == nil}
		if data == nil {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			r.Fingerprint = content.Fingerprint(st)
		}
		if err := db.Put([]index.Record{r}, false); err != nil {
			t.Fatal(err)
		}
		u, decisions, err := replica.PrepareUpload(ctx, db, source, local, store, client, []string{"file"}, f.c.Namespace(), pushOptions())
		if err != nil || u == nil || len(decisions) != 1 || decisions[0].Decision.Action != diff.Push {
			t.Fatal(u, decisions, err)
		}
		proposal := u.Proposal
		version := proposal.Entries[0].Next
		if proposal.Entries[0].Expected != previous.ID || (attempt == 1 && u.DeltaBytes[0] > 1024) {
			t.Fatal("prepared upload failed to reuse acknowledged base", u)
		}
		if preparation, err := db.PendingUploadPreparation(); err != nil || preparation != nil {
			t.Fatal("outbox did not atomically replace preparation", preparation, err)
		}
		if attempt == 1 {
			// The uploader must continue using its captured bytes, while a newer
			// observed edit remains dirty after the older upload is acknowledged.
			if err := os.WriteFile(path, []byte("new local edit"), 0600); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, true); err != nil {
				t.Fatal(err)
			}
		}
		pending, err := db.PendingUpload()
		if err != nil || pending == nil {
			t.Fatal(pending, err)
		}
		result, err := pusher.Dispatch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || !result.Committed || result.ID != proposal.ID {
			t.Fatal("dispatcher did not commit its outbox", result)
		}
		traffic := client.Traffic()
		if repeated, err := pusher.Dispatch(ctx); err != nil || repeated == nil || repeated.ID != result.ID || client.Traffic() != traffic {
			t.Fatal("durable receipt retry generated traffic", repeated, err)
		}
		if err := replica.FinishUpload(ctx, db, local, store, f.c.Namespace(), pushOptions()); err != nil {
			t.Fatal(err)
		}
		if head, pending, err := local.Head("file"); err != nil || pending || head == nil || head.ID != version.ID {
			t.Fatal("uploaded version missing from local replica", head, pending, err)
		}
		observedAfter, ok, err := db.Get("file")
		if err != nil || !ok || observedAfter.Dirty != (attempt == 1) {
			t.Fatal("dirty generation acknowledgement", observedAfter, err)
		}
		if got, err := db.Base("file"); err != nil || got == nil || got.Content.ID != version.ID {
			t.Fatal("uploaded base not saved", got, err)
		}
		// Receiving our own committed change can now complete locally from the
		// saved base, without downloading or clearing a newer source edit.
		page, err := client.ChangesPage(ctx, feedNamespace, policy, after, 1)
		if err != nil || len(page.Batches) != 1 {
			t.Fatal(page, err)
		}
		if err := db.ReceiveRemotePage(page); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteRemoteBatch(page.Batches[0].Sequence, proposal.ID); err != nil {
			t.Fatal("own upload feed entry did not complete", err)
		}
		after, feedNamespace, policy = page.Through, page.Namespace, page.Policy
		actual, err := os.ReadFile(filepath.Join(f.root, "file"))
		if data == nil {
			if !os.IsNotExist(err) || !version.Tombstone || u.DeltaBytes[0] != 0 {
				t.Fatal("prepared delete did not publish", err)
			}
		} else if err != nil || !bytes.Equal(actual, data) {
			t.Fatal("uploaded bytes differ from snapshot", err)
		}
		previous = version
	}
	dirty, err := db.DirtyPage("", 1)
	if err != nil || len(dirty) != 0 {
		t.Fatal("completed final deletion remained dirty", dirty, err)
	}
}
