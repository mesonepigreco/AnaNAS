package transferapi

import (
	"bytes"
	"context"
	"io"
	"testing"

	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
)

func TestTLSHistoricalReplayAndPathIsolation(t *testing.T) {
	f := setup(t, true, nil)
	c := newTestClient(t, clientOptions(f))
	ctx := context.Background()
	first := []byte("retained first version")
	second := []byte("new current version")
	wire := encoded(t, nil, first)
	initial, _ := f.request(t, "history-create", "file", "", first, wire)
	if _, err := c.Apply(ctx, initial, []io.Reader{bytes.NewReader(wire)}); err != nil {
		t.Fatal(err)
	}
	want := initial.Proposal.Entries[0].Next
	wire = encoded(t, first, second)
	update, _ := f.request(t, "history-edit", "file", want.ID, second, wire)
	if _, err := c.Apply(ctx, update, []io.Reader{bytes.NewReader(wire)}); err != nil {
		t.Fatal(err)
	}
	deletion := ApplyRequest{Epoch: f.c.Epoch(), Proposal: journal.Proposal{ID: identifier("history-delete"), Client: f.clientID, Entries: []journal.Entry{{Path: "file", Expected: update.Proposal.Entries[0].Next.ID, Next: journal.Version{ID: identifier("tombstone"), Tombstone: true}}}}, DeltaBytes: []int64{0}}
	if _, err := c.Apply(ctx, deletion, []io.Reader{nil}); err != nil {
		t.Fatal(err)
	}
	// The journal still names the older file, even though its current head is a
	// tombstone. Fetching that exact retained version must not resurrect the head.
	page, err := c.ChangesPage(ctx, "", "", 0, 3)
	if err != nil || len(page.Batches) != 3 || page.Batches[0].Entries[0].Next != want {
		t.Fatal(page, err)
	}
	sig, err := c.Signature(ctx, Selection{"file", want.ID}, want)
	if err != nil || sig.Digest != hash.SumBytes(first) {
		t.Fatal(sig, err)
	}
	base, err := delta.Build(ctx, bytes.NewReader(nil), 0, 65536)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := c.Download(ctx, Selection{"file", want.ID}, want, base, nil, &output); err != nil || !bytes.Equal(output.Bytes(), first) {
		t.Fatal(output.String(), err)
	}
	if _, err := c.Signature(ctx, Selection{"other-file", want.ID}, want); err == nil {
		t.Fatal("historical content exposed through another path")
	}
	head, err := c.Head(ctx, "file")
	if err != nil || head.Version == nil || !head.Version.Tombstone {
		t.Fatal("historical read changed head", head, err)
	}
}
