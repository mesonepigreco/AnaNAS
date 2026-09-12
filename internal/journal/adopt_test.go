package journal

import (
	"context"
	"errors"
	"testing"
)

func TestReplicaAdoptionAtomicAndOriginBound(t *testing.T) {
	c, _ := openTest(t)
	ctx := context.Background()
	receipt := Record{Proposal: proposal(2, "file", ""), Sequence: 99, Epoch: 7, Committed: true}
	failed := errors.New("retained content failed verification")
	if _, err := c.AdoptReplica(ctx, c.Epoch(), id(8), receipt, func(context.Context) error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if head, pending, err := c.Head("file"); err != nil || head != nil || pending {
		t.Fatal(head, pending, err)
	}
	if r, err := c.Operation(receipt.ID); err != nil || r != nil {
		t.Fatal(r, err)
	}
	r, err := c.AdoptReplica(ctx, c.Epoch(), id(8), receipt, func(context.Context) error { return nil })
	if err != nil || !r.Committed || r.Sequence != 1 || r.Epoch != c.Epoch() {
		t.Fatal("remote sequence leaked into local ordering", r, err)
	}
	refuse := func(context.Context) error { t.Fatal("unexpected verification"); return nil }
	if _, err := c.AdoptReplica(ctx, c.Epoch(), id(8), receipt, refuse); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AdoptReplica(ctx, c.Epoch(), id(9), receipt, refuse); err == nil {
		t.Fatal("replica switched remote origin")
	}
	if _, err := c.AdoptReplica(ctx, c.Epoch()+1, id(8), receipt, refuse); !errors.Is(err, ErrFence) {
		t.Fatal(err)
	}
	if v, pending, err := c.VersionAt("file", receipt.Entries[0].Next.ID); err != nil || pending || v == nil || v.ID != receipt.Entries[0].Next.ID {
		t.Fatal(v, pending, err)
	}
	if _, err := c.Commit(ctx, c.Epoch(), proposal(3, "file", receipt.Entries[0].Next.ID), success); err != nil {
		t.Fatal("adopted head unavailable to later publication", err)
	}
}
