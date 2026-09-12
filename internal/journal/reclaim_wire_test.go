package journal

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/delta"
)

func TestCommittedWireReleaseRetainsSnapshotAndRecoversPartialCleanup(t *testing.T) {
	c, dir := openTest(t)
	namespace := id(77)
	if err := c.ConfigureReplicaBudget(PublicationBudget{MaxBytes: 2 << 20, MaxEntries: 4}, namespace); err != nil {
		t.Fatal(err)
	}
	p := proposal(2, "first", "")
	p.Entries = append(p.Entries, proposal(3, "second", "").Entries...)
	if err := c.ReserveUploadCache(p); err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Entries {
		for _, suffix := range []string{".ready", ".wire.ready"} {
			if err := os.WriteFile(filepath.Join(dir, e.Next.ID+suffix), []byte("data"), 0400); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := usage(t, c)
	if err := c.ReleaseUploadWire(context.Background(), namespace, p); err == nil {
		t.Fatal("uncommitted upload cleaned")
	}
	if f, err := c.store.OpenWire(p.Entries[0].Next.ID); err != nil {
		t.Fatal("uncommitted wire removed", err)
	} else {
		f.Close()
	}
	r := Record{Proposal: p, Sequence: 1, Epoch: 1, Committed: true}
	if _, err := c.AdoptReplica(context.Background(), c.Epoch(), namespace, r, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// An unowned alias of the second spool must prevent cleanup/credit, even
	// though the first exact spool may already have been removed.
	second := filepath.Join(dir, p.Entries[1].Next.ID+".wire.ready")
	alias := filepath.Join(dir, "unowned-alias")
	if err := os.Link(second, alias); err != nil {
		t.Fatal(err)
	}
	if err := c.ReleaseUploadWire(context.Background(), namespace, p); err == nil {
		t.Fatal("unowned hard link removed")
	}
	if usage(t, c) != before {
		t.Fatal("partial cleanup credited space")
	}
	if _, err := os.Stat(filepath.Join(dir, p.Entries[0].Next.ID+".wire.ready")); !os.IsNotExist(err) {
		t.Fatal("first spool was not removed", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatal("unsafe second spool removed", err)
	}
	c.Close()
	var err error
	c, err = Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if usage(t, c) != before {
		t.Fatal("restart lost full charge")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := c.ReleaseUploadWire(context.Background(), namespace, p); err != nil {
		t.Fatal(err)
	}
	bound, _ := delta.WireBound(4)
	after := usage(t, c)
	if after.Entries != before.Entries || after.ReservedBytes != before.ReservedBytes-2*bound {
		t.Fatal("wrong wire credit", before, after)
	}
	for _, e := range p.Entries {
		if err := c.store.RequireWireVacant(e.Next.ID); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(dir, e.Next.ID+".ready")); err != nil || string(data) != "data" {
			t.Fatal("snapshot removed", err)
		}
	}
	if err := c.CheckUploadCache(p, namespace); err == nil {
		t.Fatal("released wire authorized sending")
	}
	if err := c.CheckUploadCompletion(p, namespace); err != nil {
		t.Fatal("receipt completion refused", err)
	}
	if _, err := c.AdoptReplica(context.Background(), c.Epoch(), namespace, r, func(context.Context) error { t.Fatal("idempotent adoption verified again"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.ReleaseUploadWire(context.Background(), namespace, p); err != nil || usage(t, c) != after {
		t.Fatal("repeated release changed charge", err)
	}
	// A new object under an already reclaimed name has no cleanup provenance.
	if err := os.WriteFile(second, []byte("new unowned content"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := c.ReleaseUploadWire(context.Background(), namespace, p); err == nil || usage(t, c) != after {
		t.Fatal("recreated spool cleaned/credited", err)
	}
}
