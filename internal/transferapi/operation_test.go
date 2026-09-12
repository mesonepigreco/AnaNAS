package transferapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
	"nas-sync/internal/replica"
)

func TestTLSUploadReceiptRecoveryAfterLocalRestart(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "index.db")
	d, err := index.Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := d.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	f := setupWithClientID(t, true, nil, clientID)
	c := newTestClient(t, clientOptions(f))
	ctx := context.Background()
	data := []byte("a")
	wire := encoded(t, nil, data)
	request, _ := f.request(t, "outbox", "file", "", data, wire)
	if err := d.Put([]index.Record{{Path: "file", Fingerprint: index.Fingerprint{Size: 1}}}, false); err != nil {
		t.Fatal(err)
	}
	u := index.Upload{Namespace: f.c.Namespace(), Proposal: request.Proposal, Generations: []uint64{1}, DeltaBytes: request.DeltaBytes, DeltaDigests: []hash.Digest{hash.SumBytes(wire)}}
	if err := d.PrepareUpload(u); err != nil {
		t.Fatal(err)
	}
	if prior, err := c.Operation(ctx, u.Namespace, u.Proposal.ID); err != nil || prior != nil {
		t.Fatal(prior, err)
	}
	// Deliberately omit saving the successful response, then reopen the local
	// database. This simulates missing local acknowledgement, not packet loss.
	if _, err := c.Apply(ctx, request, []io.Reader{bytes.NewReader(wire)}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = index.Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pending, err := d.PendingUpload()
	if err != nil || pending == nil {
		t.Fatal(pending, err)
	}
	record, err := c.Operation(ctx, pending.Namespace, pending.Proposal.ID)
	if err != nil || record == nil || !record.Committed || record.Sequence != 1 {
		t.Fatal(record, err)
	}
	if recovered, err := replica.RecoverUploadReceipt(ctx, d, c, func(context.Context) error { return nil }); err != nil || recovered.State != "committed" {
		t.Fatal(recovered, err)
	}
	traffic := c.Traffic()
	if recovered, err := replica.RecoverUploadReceipt(ctx, d, c, func(context.Context) error {
		t.Fatal("durable receipt should need no network policy check")
		return nil
	}); err != nil || recovered.State != "committed" {
		t.Fatal(recovered, err)
	}
	if c.Traffic() != traffic {
		t.Fatal("repeated receipt recovery generated traffic")
	}
	if err := d.CompleteUpload(record.ID); !errors.Is(err, index.ErrStale) {
		t.Fatal("receipt alone completed local work", err)
	}
	base := &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: request.Proposal.Entries[0].Next.ID, Size: 1, Blocks: []diff.Block{{Digest: hash.SumBytes(data), Size: 1}}}}
	if _, err := d.Acknowledge("file", "", 1, base); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteUpload(record.ID); err != nil {
		t.Fatal(err)
	}
	if page, err := f.c.Changes(0, 2); err != nil || len(page) != 1 {
		t.Fatal("recovery duplicated commit", page, err)
	}
	if _, err := c.Operation(ctx, identifier("wrong namespace"), record.ID); err == nil {
		t.Fatal("wrong target accepted")
	}
}

func TestOperationStatusScopedAndPending(t *testing.T) {
	f := setup(t, true, nil)
	c := newTestClient(t, clientOptions(f))
	f.api.publisher = failAfterPublish{f.publisher}
	wire := encoded(t, nil, []byte("data"))
	request, _ := f.request(t, "pending-operation", "file", "", []byte("data"), wire)
	if _, err := c.Apply(context.Background(), request, []io.Reader{bytes.NewReader(wire)}); err == nil {
		t.Fatal("injected failure missing")
	}
	record, err := c.Operation(context.Background(), f.c.Namespace(), request.Proposal.ID)
	if err != nil || record == nil || record.Committed {
		t.Fatal("pending result misreported", record, err)
	}
	if _, err := f.c.Recover(context.Background(), f.c.Epoch(), f.publisher); err != nil {
		t.Fatal(err)
	}
	foreign := journal.Proposal{ID: identifier("foreign operation"), Client: identifier("foreign client"), Entries: []journal.Entry{{Path: "foreign", Next: journal.Version{ID: identifier("foreign version"), Digest: hash.SumBytes(nil)}}}}
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), foreign, metadataPublisher{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Operation(context.Background(), f.c.Namespace(), foreign.ID); err == nil {
		t.Fatal("another client's operation exposed")
	}
}
