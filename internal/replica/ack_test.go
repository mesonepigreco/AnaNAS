package replica

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

type lostAckRemote struct {
	quietWorkerRemote
	calls [][2]uint64
}

func (r *lostAckRemote) AcknowledgeChanges(_ context.Context, _, _ string, expected, through uint64) error {
	r.calls = append(r.calls, [2]uint64{expected, through})
	if len(r.calls) == 1 {
		return errors.New("NAS committed acknowledgement but response was lost")
	}
	return nil
}

func TestWorkerRetriesLostAcknowledgementFromDurableCompletedPrefix(t *testing.T) {
	f, db, root := preparationFixture(t)
	namespace := id("remote namespace")
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}, namespace); err != nil {
		t.Fatal(err)
	}
	r := journal.Record{Proposal: journal.Proposal{ID: id("ack batch"), Client: id("author"), Entries: []journal.Entry{{Path: "hidden", Next: journal.Version{ID: id("hidden-version"), Digest: hash.SumBytes(nil)}}}}, Sequence: 1, Epoch: 1, Committed: true}
	page, err := changefeed.Filter(namespace, nil, changefeed.Request{Limit: 1, Exclusions: []string{"hidden"}}, []journal.Record{r})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	remote := &lostAckRemote{}
	opts := WorkerOptions{Namespace: namespace, PushOptions: completionOptions(), Ready: func() bool { return true }}
	opts.Exclusions = []string{"hidden"}
	w, err := NewWorker(db, root, f.c, f.store, f.publisher, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	state, err := db.RemoteState()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.acknowledge(context.Background(), state); err != nil || len(remote.calls) != 0 {
		t.Fatal("received-only batch sent ACK", err)
	}
	if err := db.CompleteRemoteBatch(1, r.ID); err != nil {
		t.Fatal(err)
	}
	state, err = db.RemoteState()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.acknowledge(context.Background(), state); err == nil {
		t.Fatal("lost response not surfaced")
	}
	db.Close()
	db, err = index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w, err = NewWorker(db, root, f.c, f.store, f.publisher, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	state, err = db.RemoteState()
	if err != nil || state.Acknowledged != 0 || state.Completed != 1 {
		t.Fatal("lost response invented confirmed cursor", state, err)
	}
	if err := w.acknowledge(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(remote.calls) != 2 || remote.calls[0] != ([2]uint64{0, 1}) || remote.calls[1] != remote.calls[0] {
		t.Fatal("restart changed ACK range", remote.calls)
	}
	state, err = db.RemoteState()
	if err != nil || state.Acknowledged != 1 {
		t.Fatal(state, err)
	}
	if err := w.acknowledge(context.Background(), state); err != nil || len(remote.calls) != 2 {
		t.Fatal("caught-up worker sent another ACK", err)
	}
}
