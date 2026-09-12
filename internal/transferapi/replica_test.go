package transferapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/diff"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/publish"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

func TestTLSDownloadPublishesIndependentReplica(t *testing.T) {
	server := setup(t, true, nil)
	client := newTestClient(t, clientOptions(server))
	dir := t.TempDir()
	root, state := filepath.Join(dir, "root"), filepath.Join(dir, "state")
	for _, path := range []string{root, state} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c, err := journal.Open(state, identifier("local-replica"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db, err := index.Open(filepath.Join(state, "index.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := stage.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	publisher, err := publish.Open(root, state, c.Version, nil, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	puller, err := replica.NewPuller(c, store, publisher, client, replica.PullOptions{Namespace: server.c.Namespace(), Writes: true, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base := make([]byte, 256*1024)
	if _, err := rand.Read(base); err != nil {
		t.Fatal(err)
	}
	wire := encoded(t, nil, base)
	first, _ := server.request(t, "replica-create", "file", "", base, wire)
	if _, err := client.Apply(ctx, first, []io.Reader{bytes.NewReader(wire)}); err != nil {
		t.Fatal(err)
	}
	page, err := client.ChangesPage(ctx, "", "", 0, 1)
	if err != nil || len(page.Batches) != 1 {
		t.Fatal(page, err)
	}
	if err := db.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	if result, err := puller.Pull(ctx, page.Batches[0].Proposal); err != nil || !result.Record.Committed {
		t.Fatal(result, err)
	}
	if err := replica.FinishDownload(ctx, db, c, store, server.c.Namespace(), pushOptions()); err != nil {
		t.Fatal(err)
	}
	// Simulate the observer reporting publication. Completion must leave this
	// dirty record queued rather than confuse it with a known upload generation.
	st, err := os.Stat(filepath.Join(root, "file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, true); err != nil {
		t.Fatal(err)
	}
	target := append([]byte{23}, base...)
	wire = encoded(t, base, target)
	second, _ := server.request(t, "replica-edit", "file", first.Proposal.Entries[0].Next.ID, target, wire)
	if _, err := client.Apply(ctx, second, []io.Reader{bytes.NewReader(wire)}); err != nil {
		t.Fatal(err)
	}
	page, err = client.ChangesPage(ctx, page.Namespace, page.Policy, page.Through, 1)
	if err != nil || len(page.Batches) != 1 {
		t.Fatal(page, err)
	}
	if err := db.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	before := client.Traffic()
	result, err := puller.Pull(ctx, page.Batches[0].Proposal)
	if err != nil || result.Transfers[0].LiteralBytes != 1 || result.Transfers[0].ReusedBytes != int64(len(base)) {
		t.Fatal(result, err)
	}
	for _, root := range []string{root, server.root} {
		actual, err := os.ReadFile(filepath.Join(root, "file"))
		if err != nil || !bytes.Equal(actual, target) {
			t.Fatal("replica bytes differ", root, err)
		}
	}
	after := client.Traffic()
	if after.Received <= before.Received || after.Received-before.Received >= uint64(len(base)) {
		t.Fatal("pull did not save transport bytes", before, after)
	}
	if err := replica.FinishDownload(ctx, db, c, store, server.c.Namespace(), pushOptions()); err != nil {
		t.Fatal(err)
	}
	if saved, err := db.Base("file"); err != nil || saved == nil || saved.Content.ID != second.Proposal.Entries[0].Next.ID {
		t.Fatal("download base not saved", saved, err)
	}
	if dirty, err := db.DirtyPage("", 1); err != nil || len(dirty) != 1 {
		t.Fatal("download completion erased observed work", dirty, err)
	}
	st, err = os.Stat(filepath.Join(root, "file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, true); err != nil {
		t.Fatal(err)
	}
	source, err := content.OpenRoot(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	compared, err := replica.Compare(ctx, db, source, c, client, "file", replica.ComparisonOptions{Namespace: server.c.Namespace(), MaxFileBytes: 1 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }})
	if err != nil || !compared.Cleared || compared.Decision.Action != diff.Noop {
		t.Fatal("publication event did not reconcile to saved base", compared, err)
	}
	if client.Traffic() != after {
		t.Fatal("unchanged downloaded content generated traffic")
	}
	// Retrying committed local work does not contact the remote transport.
	if _, err := puller.Pull(ctx, page.Batches[0].Proposal); err != nil {
		t.Fatal(err)
	}
	if current := client.Traffic(); current != after {
		t.Fatal("local retry sent traffic", current, after)
	}
	deletion := ApplyRequest{Epoch: server.c.Epoch(), Proposal: journal.Proposal{ID: identifier("replica-delete"), Client: server.clientID, Entries: []journal.Entry{{Path: "file", Expected: second.Proposal.Entries[0].Next.ID, Next: journal.Version{ID: identifier("replica-tombstone"), Tombstone: true}}}}, DeltaBytes: []int64{0}}
	if _, err := client.Apply(ctx, deletion, []io.Reader{nil}); err != nil {
		t.Fatal(err)
	}
	page, err = client.ChangesPage(ctx, page.Namespace, page.Policy, page.Through, 1)
	if err != nil || len(page.Batches) != 1 {
		t.Fatal(page, err)
	}
	if err := db.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	if _, err := puller.Pull(ctx, deletion.Proposal); err != nil {
		t.Fatal(err)
	}
	if err := replica.FinishDownload(ctx, db, c, store, server.c.Namespace(), pushOptions()); err != nil {
		t.Fatal(err)
	}
	if saved, err := db.Base("file"); err != nil || saved == nil || !saved.Content.Tombstone {
		t.Fatal("explicit deletion base not saved", saved, err)
	}
	if s, err := db.RemoteState(); err != nil || s.Completed != 3 {
		t.Fatal("download inbox did not complete", s, err)
	}
	if _, err := os.Stat(filepath.Join(root, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("explicit delete not applied", err)
	}
}
