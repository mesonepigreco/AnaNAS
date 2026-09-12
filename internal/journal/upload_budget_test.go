package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/delta"
)

func TestUploadBudgetRestartAbortAndAdoption(t *testing.T) {
	c, dir := openTest(t)
	namespace := id(77)
	limits := PublicationBudget{MaxBytes: 4 << 20, MaxEntries: 4}
	if err := c.ConfigureReplicaBudget(limits, namespace); err != nil {
		t.Fatal(err)
	}
	p := proposal(2, "file", "")
	if err := c.ReserveUploadCache(p); err != nil {
		t.Fatal(err)
	}
	before := usage(t, c)
	wire, _ := delta.WireBound(4)
	if before.ReservedBytes != (192<<10)+4+wire || before.Entries != 1 || before.ReplicaNamespace != namespace {
		t.Fatal(before)
	}
	if err := c.ReserveUploadCache(p); err != nil || usage(t, c) != before {
		t.Fatal("retry changed charge", err)
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); !errors.Is(err, ErrIdentity) {
		t.Fatal("upload reservation reused for publication", err)
	}
	path := filepath.Join(dir, p.Entries[0].Next.ID+".partial")
	if err := os.WriteFile(path, []byte("da"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Close()
	var err error
	c, err = Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if usage(t, c) != before {
		t.Fatal("restart lost upload charge")
	}
	if err := c.ConfigurePublicationBudget(limits); err == nil {
		t.Fatal("changed persisted budget mode")
	}
	failure := errors.New("cleanup fsync failed")
	if err := c.AbortUploadCache(p, func() error { return failure }); !errors.Is(err, failure) || usage(t, c) != before {
		t.Fatal("failed cleanup credited bytes", err)
	}
	if err := c.AbortUploadCache(p, func() error { return nil }); err == nil || usage(t, c) != before {
		t.Fatal("remaining candidate credited", err)
	}
	if err := c.AbortUploadCache(p, func() error { return c.store.AbortUncommittedUpload(p.Entries[0].Next.ID) }); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, c); u.Entries != 0 || u.ReservedBytes != 0 {
		t.Fatal("abort not credited", u)
	}
	if err := c.AbortUploadCache(p, func() error { t.Fatal("abort retry invoked cleanup"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.ReserveUploadCache(p); err != nil {
		t.Fatal(err)
	}
	receipt := Record{Proposal: p, Sequence: 1, Epoch: 1, Committed: true}
	if _, err := c.AdoptReplica(context.Background(), c.Epoch(), namespace, receipt, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if usage(t, c) != before {
		t.Fatal("adoption changed retained charge")
	}
	if _, err := c.AdoptReplica(context.Background(), c.Epoch(), namespace, receipt, func(context.Context) error { t.Fatal("repeat adoption verified again"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.AbortUploadCache(p, func() error { t.Fatal("committed upload cleanup reached"); return nil }); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
	other := Record{Proposal: proposal(3, "other", ""), Sequence: 2, Epoch: 1, Committed: true}
	if _, err := c.AdoptReplica(context.Background(), c.Epoch(), namespace, other, func(context.Context) error { t.Fatal("unreserved adoption verified"); return nil }); err == nil {
		t.Fatal("unreserved upload adopted")
	}
	// Received publications and uploads consume the same remaining capacity.
	if _, err := c.Commit(context.Background(), c.Epoch(), other.Proposal, success); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, c); u.Entries != 2 || u.ReservedBytes != before.ReservedBytes+(192<<10)+8 {
		t.Fatal("download did not share budget", u)
	}
}

func TestUploadBudgetIdentityAndMissingReservation(t *testing.T) {
	c, dir := openTest(t)
	if err := c.ConfigureReplicaBudget(PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 1}, id(77)); err != nil {
		t.Fatal(err)
	}
	p := proposal(2, "file", "")
	if err := c.ReserveUploadCache(p); err != nil {
		t.Fatal(err)
	}
	other := proposal(3, "other", "")
	if err := c.ReserveUploadCache(other); !errors.Is(err, ErrBudget) {
		t.Fatal("entry cap bypassed", err)
	}
	changed := clone(Record{Proposal: p}).Proposal
	changed.Entries[0].Next.Size++
	if err := c.ReserveUploadCache(changed); !errors.Is(err, ErrIdentity) {
		t.Fatal("changed size accepted", err)
	}
	if err := c.AbortUploadCache(changed, func() error { t.Fatal("different reservation cleaned"); return nil }); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
	path := filepath.Join(dir, other.Entries[0].Next.ID+".ready")
	if err := os.WriteFile(path, []byte("keep"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := c.AbortUploadCache(other, func() error { t.Fatal("unreserved content cleaned"); return nil }); err == nil {
		t.Fatal("unreserved content accepted")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "keep" {
		t.Fatal("unreserved content changed", err)
	}
}
