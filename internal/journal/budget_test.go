package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/hash"
)

func usage(t *testing.T, c *Coordinator) BudgetUsage {
	t.Helper()
	u, err := c.PublicationBudgetUsage()
	if err != nil || u == nil {
		t.Fatal(u, err)
	}
	return *u
}

func TestBudgetRefusesBeforeIntakeAndPublication(t *testing.T) {
	for _, intake := range []bool{false, true} {
		c, _ := openTest(t)
		limit := PublicationBudget{MaxBytes: 192 << 10, MaxEntries: 1}
		if err := c.ConfigurePublicationBudget(limit); err != nil {
			t.Fatal(err)
		}
		p := proposal(2, "file", "") // eight bytes above the allowance
		publisher := publishFunc(func(context.Context, Record) error { t.Fatal("budget refusal reached publisher"); return nil })
		var err error
		if intake {
			_, err = c.StageAndCommit(context.Background(), c.Epoch(), p, func(context.Context, bool) error { t.Fatal("budget refusal reached receiver"); return nil }, publisher)
		} else {
			_, err = c.Commit(context.Background(), c.Epoch(), p, publisher)
		}
		if !errors.Is(err, ErrBudget) {
			t.Fatal(err)
		}
		if u := usage(t, c); u.ReservedBytes != 0 || u.Entries != 0 {
			t.Fatal("refused batch consumed budget", u)
		}
		if err := c.db.View(func(tx *bolt.Tx) error {
			in, err := readIntake(tx)
			if err != nil || in != nil || tx.Bucket(metaBucket).Get([]byte("pending")) != nil {
				t.Fatal("refused batch retained ownership", in, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBudgetRestartReservesOnceAndRetainsCommittedHistory(t *testing.T) {
	c, dir := openTest(t)
	b := PublicationBudget{MaxBytes: 2 << 20, MaxEntries: 2}
	if err := c.ConfigurePublicationBudget(b); err != nil {
		t.Fatal(err)
	}
	p := proposal(2, "file", "")
	failure := errors.New("connection lost")
	_, err := c.StageAndCommit(context.Background(), c.Epoch(), p, func(context.Context, bool) error { return failure }, success)
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	u := usage(t, c)
	if u.ReservedBytes != (192<<10)+8 || u.Entries != 1 {
		t.Fatal(u)
	}
	c.Close()
	c, err = Open(dir, id(99)) // no reconfiguration: budget remains active
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if usage(t, c) != u {
		t.Fatal("restart lost reservation")
	}
	if err := c.ConfigurePublicationBudget(b); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigurePublicationBudget(PublicationBudget{MaxBytes: 3 << 20, MaxEntries: 2}); err != nil {
		t.Fatal("monotonic byte increase refused", err)
	}
	u = usage(t, c)
	if u.MaxBytes != 3<<20 || u.MaxEntries != 2 || u.ReservedBytes != (192<<10)+8 || u.Entries != 1 {
		t.Fatal("budget increase changed usage", u)
	}
	if err := c.ConfigurePublicationBudget(b); err == nil {
		t.Fatal("budget decrease accepted")
	}
	if err := c.ConfigurePublicationBudget(PublicationBudget{MaxBytes: 4 << 20, MaxEntries: 1}); err == nil {
		t.Fatal("mixed increase/decrease accepted")
	}
	_, err = c.StageAndCommit(context.Background(), c.Epoch(), p, func(_ context.Context, resumed bool) error {
		if !resumed {
			t.Fatal("lost intake")
		}
		return nil
	}, success)
	if err != nil || usage(t, c) != u {
		t.Fatal("retry double charged or credited retained content", err)
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil || usage(t, c) != u {
		t.Fatal("committed retry changed charge", err)
	}
	deletion := proposal(3, "file", p.Entries[0].Next.ID)
	deletion.Entries[0].Next = Version{ID: id(1003), Tombstone: true}
	if _, err := c.Commit(context.Background(), c.Epoch(), deletion, success); err != nil {
		t.Fatal(err)
	}
	u = usage(t, c)
	if u.ReservedBytes != 2*(192<<10)+12 || u.Entries != 2 {
		t.Fatal("displaced old base not charged", u)
	}
	empty := proposal(4, "empty", "")
	empty.Entries[0].Next.Size, empty.Entries[0].Next.Digest = 0, hash.SumBytes(nil)
	if _, err := c.Commit(context.Background(), c.Epoch(), empty, success); !errors.Is(err, ErrBudget) {
		t.Fatal("zero-byte objects bypassed entry cap", err)
	}
}

func TestBudgetPublicationFailureAndConflict(t *testing.T) {
	c, _ := openTest(t)
	if err := c.ConfigurePublicationBudget(PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}); err != nil {
		t.Fatal(err)
	}
	p := proposal(2, "file", "")
	failure := errors.New("fsync failed")
	_, err := c.Commit(context.Background(), c.Epoch(), p, publishFunc(func(context.Context, Record) error { return failure }))
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	u := usage(t, c)
	if _, err := c.Recover(context.Background(), c.Epoch(), success); err != nil || usage(t, c) != u {
		t.Fatal("recovery changed charge", err)
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), proposal(3, "file", ""), success); !errors.Is(err, ErrConflict) || usage(t, c) != u {
		t.Fatal("conflict consumed budget", err)
	}
	if err := c.BindReplicaOrigin(id(88)); err == nil {
		t.Fatal("receiver budget used as replica spool budget")
	}
	receipt := Record{Proposal: proposal(4, "other", ""), Sequence: 1, Epoch: 1, Committed: true}
	if _, err := c.AdoptReplica(context.Background(), c.Epoch(), id(88), receipt, func(context.Context) error { t.Fatal("budget bypass reached verifier"); return nil }); err == nil {
		t.Fatal("adoption bypassed budget")
	}
}

func TestBudgetBootstrapRefusesUnaccountedState(t *testing.T) {
	for _, kind := range []string{"ready", "directory", "symlink", "history", "intake", "replica"} {
		t.Run(kind, func(t *testing.T) {
			c, dir := openTest(t)
			switch kind {
			case "ready":
				if err := os.WriteFile(filepath.Join(dir, id(4)+".ready"), []byte("keep"), 0400); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(filepath.Join(dir, "keep"), 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("missing", filepath.Join(dir, "keep")); err != nil {
					t.Fatal(err)
				}
			case "history":
				if _, err := c.Commit(context.Background(), c.Epoch(), proposal(2, "file", ""), success); err != nil {
					t.Fatal(err)
				}
			case "intake":
				_, _ = c.StageAndCommit(context.Background(), c.Epoch(), proposal(2, "file", ""), func(context.Context, bool) error { return errors.New("interrupted") }, success)
			case "replica":
				if err := c.BindReplicaOrigin(id(88)); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.ConfigurePublicationBudget(PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}); err == nil {
				t.Fatal("adopted unaccounted state")
			}
			if u, err := c.PublicationBudgetUsage(); err != nil || u != nil {
				t.Fatal("failed bootstrap persisted", u, err)
			}
		})
	}
}
