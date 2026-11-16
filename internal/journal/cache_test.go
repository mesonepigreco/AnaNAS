package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func cacheFixture(t *testing.T) (*Coordinator, string, Proposal, Proposal) {
	t.Helper()
	c, dir := openTest(t)
	if err := c.ConfigureReplicaBudget(PublicationBudget{MaxBytes: 10 << 20, MaxEntries: 100}, id(77)); err != nil {
		t.Fatal(err)
	}
	a := proposal(2, "file", "")
	b := proposal(3, "file", a.Entries[0].Next.ID)
	for _, p := range []Proposal{a, b} {
		if err := os.WriteFile(filepath.Join(dir, p.Entries[0].Next.ID+".ready"), []byte("data"), 0400); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil {
			t.Fatal(err)
		}
	}
	return c, dir, a, b
}

func noCachePins(_ context.Context, _ []Entry, f func(map[string]bool) error) error {
	return f(map[string]bool{})
}

func TestReplicaCacheRetainsCurrentMetadataAndCreditsOnce(t *testing.T) {
	c, dir, a, b := cacheFixture(t)
	before := usage(t, c)
	orphan := filepath.Join(dir, id(555)+".ready")
	if err := os.WriteFile(orphan, []byte("unowned"), 0400); err != nil {
		t.Fatal(err)
	}
	r, err := c.MaintainReplicaCache(context.Background(), id(77), "", 4096, false, noCachePins)
	if err != nil || r.Candidates != 1 || r.Removed != 0 || r.LogicalBytes != 4 || usage(t, c) != before {
		t.Fatal(r, err)
	}
	if len(r.Details) != 1 || r.Details[0].Version != a.Entries[0].Next.ID || r.Details[0].Path != "file" {
		t.Fatal("candidate manifest differs", r.Details)
	}
	for i := 0; i < 2; i++ {
		r, err = c.MaintainReplicaCache(context.Background(), id(77), "", 4096, true, noCachePins)
		want := 1 - i
		if err != nil || r.Removed != want {
			t.Fatal(r, err)
		}
		if u := usage(t, c); u.ReservedBytes != before.ReservedBytes-4 || u.Entries != before.Entries {
			t.Fatal(u)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, a.Entries[0].Next.ID+".ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, p := range []string{orphan, filepath.Join(dir, b.Entries[0].Next.ID+".ready")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	if v, _, err := c.VersionAt("file", a.Entries[0].Next.ID); err != nil || v == nil {
		t.Fatal(v, err)
	}
	if rows, err := c.Changes(0, 10); err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), a, success); err != nil {
		t.Fatal("idempotent metadata retry", err)
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), proposal(4, "file", b.Entries[0].Next.ID), success); err != nil {
		t.Fatal("future update", err)
	}
}

func TestReplicaCachePinsPendingAndRoleBoundaries(t *testing.T) {
	c, dir, a, _ := cacheFixture(t)
	pinned := func(_ context.Context, _ []Entry, f func(map[string]bool) error) error {
		return f(map[string]bool{a.Entries[0].Next.ID: true})
	}
	if r, err := c.MaintainReplicaCache(context.Background(), id(77), "", 10, true, pinned); err != nil || r.Removed != 0 {
		t.Fatal(r, err)
	}
	if _, err := c.MaintainReplicaCache(context.Background(), id(78), "", 10, true, noCachePins); err == nil {
		t.Fatal("wrong namespace accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, a.Entries[0].Next.ID+".ready")); err != nil {
		t.Fatal(err)
	}
	_, err := c.StageAndCommit(context.Background(), c.Epoch(), proposal(9, "other", ""), func(context.Context, bool) error { return errors.New("interrupted intake") }, success)
	if err == nil {
		t.Fatal("fixture did not stop")
	}
	if r, err := c.MaintainReplicaCache(context.Background(), id(77), "", 10, true, noCachePins); err != nil || r.Removed != 1 {
		t.Fatal(r, err)
	}
	if pending, err := c.PendingOperation(); err != nil || !pending {
		t.Fatal("unrelated recovery was changed", pending, err)
	}
	native, _ := openTest(t)
	if err := native.ConfigurePublicationBudget(PublicationBudget{MaxBytes: 10 << 20, MaxEntries: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := native.MaintainReplicaCache(context.Background(), id(77), "", 10, true, noCachePins); err == nil {
		t.Fatal("NAS helper history accepted")
	}
}

func TestReplicaCacheCrashAfterUnlinkRecoversAndFencesWrites(t *testing.T) {
	c, dir, a, b := cacheFixture(t)
	before := usage(t, c)
	raw, _ := json.Marshal([]Version{a.Entries[0].Next})
	if err := c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(metaBucket).Put(cachePendingKey, raw) }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, a.Entries[0].Next.ID+".ready")); err != nil {
		t.Fatal(err)
	}
	c.Close()
	var err error
	c, err = Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Commit(context.Background(), c.Epoch(), proposal(4, "file", b.Entries[0].Next.ID), success); !errors.Is(err, ErrPending) {
		t.Fatal("incomplete cleanup did not fence writes", err)
	}
	if r, err := c.MaintainReplicaCache(context.Background(), id(77), "", 10, true, noCachePins); err != nil || r.Removed != 1 {
		t.Fatal(r, err)
	}
	if u := usage(t, c); u.ReservedBytes != before.ReservedBytes-4 {
		t.Fatal(u)
	}
	if r, err := c.MaintainReplicaCache(context.Background(), id(77), "", 10, true, noCachePins); err != nil || r.Removed != 0 {
		t.Fatal(r, err)
	}
}

func TestReplicaCacheRefusesAlteredArtifactsAndNeverCreditsMissing(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "size", "mode", "missing"} {
		t.Run(kind, func(t *testing.T) {
			c, dir, a, _ := cacheFixture(t)
			before := usage(t, c)
			p := filepath.Join(dir, a.Entries[0].Next.ID+".ready")
			switch kind {
			case "symlink":
				os.Remove(p)
				if err := os.Symlink("/etc/passwd", p); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(p, filepath.Join(dir, "unowned-alias")); err != nil {
					t.Fatal(err)
				}
			case "size":
				os.Chmod(p, 0600)
				os.WriteFile(p, []byte("different size"), 0400)
				os.Chmod(p, 0400)
			case "mode":
				os.Chmod(p, 0600)
			case "missing":
				os.Remove(p)
			}
			r, err := c.MaintainReplicaCache(context.Background(), id(77), "", 10, true, noCachePins)
			if kind == "missing" {
				if err != nil || r.Missing != 1 {
					t.Fatal(r, err)
				}
			} else if err == nil {
				t.Fatal("unsafe artifact removed", kind)
			}
			if usage(t, c) != before {
				t.Fatal("unproven removal credited")
			}
		})
	}
}
