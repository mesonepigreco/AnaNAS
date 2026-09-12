package journal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestIntakeRestartIdentityAndPublicationBoundary(t *testing.T) {
	c, dir := openTest(t)
	p := proposal(2, "file", "")
	failure := errors.New("interrupted input")
	epoch := c.Epoch()
	_, err := c.StageAndCommit(context.Background(), epoch, p, func(_ context.Context, resumed bool) error {
		if resumed {
			t.Fatal("new upload was treated as owned")
		}
		if err := c.db.View(func(tx *bolt.Tx) error {
			intake, err := readIntake(tx)
			if err != nil || intake == nil || intake.ID != p.ID {
				t.Fatal("input preceded durable ownership", intake, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return failure
	}, success)
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if v, pending, err := c.Head("file"); err != nil || pending || v != nil {
		t.Fatal("intake claimed visible publication", v, pending, err)
	}
	if r, err := c.Operation(p.ID); err != nil || r != nil {
		t.Fatal("intake appeared prepared", r, err)
	}
	if changes, err := c.Changes(0, 1); err != nil || len(changes) != 0 {
		t.Fatal(changes, err)
	}
	refuse := func(context.Context, bool) error { t.Fatal("unrelated input reached receiver"); return nil }
	if _, err := c.Commit(context.Background(), epoch, proposal(3, "other", ""), success); !errors.Is(err, ErrPending) {
		t.Fatal("commit bypassed intake ownership", err)
	}
	other := p
	other.Client = id(7)
	if _, err := c.StageAndCommit(context.Background(), epoch, other, refuse, success); !errors.Is(err, ErrIdentity) {
		t.Fatal("different client claimed intake", err)
	}
	c.Close()
	c, err = Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.StageAndCommit(context.Background(), epoch, p, refuse, success); !errors.Is(err, ErrFence) {
		t.Fatal(err)
	}
	if _, err := c.StageAndCommit(context.Background(), c.Epoch(), proposal(3, "other", ""), refuse, success); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	record, err := c.StageAndCommit(context.Background(), c.Epoch(), p, func(_ context.Context, resumed bool) error {
		if !resumed {
			t.Fatal("restart lost intake")
		}
		return nil
	}, publishFunc(func(_ context.Context, r Record) error {
		return c.db.View(func(tx *bolt.Tx) error {
			intake, err := readIntake(tx)
			if err != nil || intake != nil || string(tx.Bucket(metaBucket).Get([]byte("pending"))) != r.ID {
				t.Fatal("non-atomic intake to publication", intake, err)
			}
			return nil
		})
	}))
	if err != nil || !record.Committed || record.Sequence != 1 {
		t.Fatal(record, err)
	}
	if _, err := c.StageAndCommit(context.Background(), c.Epoch(), p, refuse, success); err != nil {
		t.Fatal("committed retry received content", err)
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), proposal(3, "other", ""), success); err != nil {
		t.Fatal("completed intake blocked next commit", err)
	}
}

func TestIntakeNeverAdoptsUnownedArtifacts(t *testing.T) {
	for _, suffix := range []string{".partial", ".ready"} {
		t.Run(suffix, func(t *testing.T) {
			c, dir := openTest(t)
			p := proposal(2, "file", "")
			path := filepath.Join(dir, p.Entries[0].Next.ID+suffix)
			if err := os.WriteFile(path, []byte("unowned"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := c.StageAndCommit(context.Background(), c.Epoch(), p, func(context.Context, bool) error { t.Fatal("adopted unowned candidate"); return nil }, success); !errors.Is(err, os.ErrExist) {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "unowned" {
				t.Fatal("unowned candidate modified", err)
			}
			if err := c.db.View(func(tx *bolt.Tx) error {
				p, err := readIntake(tx)
				if err != nil || p != nil {
					t.Fatal("failed reservation persisted", p, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKilledIntakeRetainsPartialOwnership(t *testing.T) {
	if dir := os.Getenv("ANANAS_INTAKE_CRASH_TEST"); dir != "" {
		c, err := Open(dir, id(99))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.ConfigurePublicationBudget(PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}); err != nil {
			t.Fatal(err)
		}
		p := proposal(2, "file", "")
		_, err = c.StageAndCommit(context.Background(), c.Epoch(), p, func(ctx context.Context, _ bool) error {
			return c.store.ReceiveVerified(ctx, p.Entries[0].Next.ID, 4, p.Entries[0].Next.Digest, func(out io.Writer) error {
				if _, err := out.Write([]byte("da")); err != nil {
					return err
				}
				fmt.Fprintln(os.Stdout, "intake-partial")
				time.Sleep(time.Hour)
				return errors.New("crash fixture was not killed")
			})
		}, success)
		t.Fatal("crash fixture returned", err)
	}
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKilledIntakeRetainsPartialOwnership$")
	cmd.Env = append(os.Environ(), "ANANAS_INTAKE_CRASH_TEST="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "intake-partial\n" {
		t.Fatal(line, err)
	}
	if other, err := Open(dir, id(99)); err == nil {
		other.Close()
		t.Fatal("live intake lock stolen")
	}
	cmd.Process.Kill()
	cmd.Wait()
	c, err := Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := proposal(2, "file", "")
	partial := filepath.Join(dir, p.Entries[0].Next.ID+".partial")
	beforeBudget := usage(t, c)
	if beforeBudget.Entries != 1 || beforeBudget.ReservedBytes != (192<<10)+8 {
		t.Fatal("killed receiver lost reservation", beforeBudget)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatal("killed partial missing", err)
	}
	r, err := c.StageAndCommit(ctx, c.Epoch(), p, func(ctx context.Context, resume bool) error {
		if !resume {
			t.Fatal("lost killed receiver ownership")
		}
		if err := c.store.DiscardPartial(p.Entries[0].Next.ID); err != nil {
			return err
		}
		return c.store.ReceiveVerified(ctx, p.Entries[0].Next.ID, 4, p.Entries[0].Next.Digest, func(w io.Writer) error { _, err := w.Write([]byte("data")); return err })
	}, success)
	if err != nil || !r.Committed {
		t.Fatal(r, err)
	}
	if usage(t, c) != beforeBudget {
		t.Fatal("killed receiver recovery changed its charge")
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal("owned partial remained", err)
	}
}
