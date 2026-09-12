package journal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/hash"
)

func id(n int) string { return fmt.Sprintf("%064x", n) }
func proposal(n int, path, expected string) Proposal {
	return Proposal{ID: id(n), Client: id(1), Entries: []Entry{{Path: path, Expected: expected, Next: Version{ID: id(n + 1000), Size: 4, Digest: hash.SumBytes([]byte("data"))}}}}
}

type publishFunc func(context.Context, Record) error

func (f publishFunc) Publish(ctx context.Context, r Record) error { return f(ctx, r) }

var success = publishFunc(func(context.Context, Record) error { return nil })

func openTest(t *testing.T) (*Coordinator, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, dir
}

func TestCommitAndIdempotence(t *testing.T) {
	c, _ := openTest(t)
	p := proposal(2, "file", "")
	calls := 0
	r, err := c.Commit(context.Background(), c.Epoch(), p, publishFunc(func(ctx context.Context, r Record) error {
		calls++
		if r.Committed || r.Sequence != 1 {
			t.Fatal("incorrect prepared record", r)
		}
		// Reading committed state is permitted here; reentrant mutations are not.
		v, pending, err := c.Head("file")
		if err != nil || v != nil || !pending {
			t.Fatal("publication preceded durable preparation", v, pending, err)
		}
		// Caller cannot mutate the coordinator's record through the callback.
		r.Entries[0].Path = "modified"
		return nil
	}))
	if err != nil || !r.Committed || r.Sequence != 1 {
		t.Fatal(r, err)
	}
	v, pending, err := c.Head("file")
	if err != nil || pending || v == nil || v.ID != p.Entries[0].Next.ID {
		t.Fatal(v, pending, err)
	}
	r, err = c.Commit(context.Background(), c.Epoch(), p, publishFunc(func(context.Context, Record) error { calls++; return nil }))
	if err != nil || r.Sequence != 1 || calls != 1 {
		t.Fatal("duplicate was republished", r, calls, err)
	}
	p.Entries[0].Path = "different"
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); !errors.Is(err, ErrIdentity) {
		t.Fatal("reused operation accepted", err)
	}
}

func TestConflictsAndAtomicBatchValidation(t *testing.T) {
	c, _ := openTest(t)
	p := proposal(2, "file", "")
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil {
		t.Fatal(err)
	}
	called := false
	failIfCalled := publishFunc(func(context.Context, Record) error { called = true; return nil })
	if _, err := c.Commit(context.Background(), c.Epoch(), proposal(3, "file", ""), failIfCalled); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	batch := proposal(4, "new", "")
	batch.Entries = append(batch.Entries, proposal(5, "file", "").Entries...)
	if _, err := c.Commit(context.Background(), c.Epoch(), batch, failIfCalled); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if called {
		t.Fatal("conflicting batch reached publisher")
	}
	if v, pending, err := c.Head("new"); err != nil || v != nil || pending {
		t.Fatal("partial batch committed", v, pending, err)
	}
	duplicate := proposal(6, "other", "")
	duplicate.Entries[0].Next = p.Entries[0].Next
	if _, err := c.Commit(context.Background(), c.Epoch(), duplicate, failIfCalled); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
	if called {
		t.Fatal("immutable identity reuse reached publisher")
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), proposal(7, "file", p.Entries[0].Next.ID), success); err != nil {
		t.Fatal(err)
	}
}

func TestPendingRecoveryAndEpoch(t *testing.T) {
	c, dir := openTest(t)
	epoch := c.Epoch()
	p := proposal(2, "file", "")
	failure := errors.New("publication interrupted")
	if _, err := c.Commit(context.Background(), epoch, p, publishFunc(func(context.Context, Record) error { return failure })); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if page, err := c.Changes(0, 10); err != nil || len(page) != 0 {
		t.Fatal("prepared batch exposed as committed", page, err)
	}
	if _, err := c.Commit(context.Background(), epoch, proposal(3, "other", ""), success); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if err := c.Acknowledge(id(1), 0, 1); err == nil {
		t.Fatal("acknowledged uncommitted work")
	}
	c.Close()
	reopened, err := Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Epoch() <= epoch {
		t.Fatal("epoch did not advance")
	}
	if _, err := reopened.Recover(context.Background(), epoch, success); !errors.Is(err, ErrFence) {
		t.Fatal("stale recovery epoch accepted", err)
	}
	r, err := reopened.Recover(context.Background(), reopened.Epoch(), publishFunc(func(_ context.Context, r Record) error {
		if r.ID != p.ID || r.Epoch != epoch || r.Sequence != 1 {
			t.Fatal("recovery lost original transaction", r)
		}
		return nil
	}))
	if err != nil || r == nil || !r.Committed {
		t.Fatal(r, err)
	}
	if next, err := reopened.Recover(context.Background(), reopened.Epoch(), success); err != nil || next != nil {
		t.Fatal("repeated recovery published again", next, err)
	}
	if _, err := reopened.Commit(context.Background(), epoch, proposal(3, "other", ""), success); !errors.Is(err, ErrFence) {
		t.Fatal(err)
	}
	if _, err := reopened.Commit(context.Background(), reopened.Epoch(), proposal(3, "other", ""), success); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentClientsSerializeAndConflict(t *testing.T) {
	c, _ := openTest(t)
	var wg sync.WaitGroup
	var committed, conflicted, published atomic.Int32
	for n := 2; n < 18; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			p := proposal(n, "same", "")
			p.Client = id(n)
			_, err := c.Commit(context.Background(), c.Epoch(), p, publishFunc(func(context.Context, Record) error { published.Add(1); return nil }))
			if err == nil {
				committed.Add(1)
			} else if errors.Is(err, ErrConflict) {
				conflicted.Add(1)
			} else {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	if committed.Load() != 1 || published.Load() != 1 || conflicted.Load() != 15 {
		t.Fatal(committed.Load(), published.Load(), conflicted.Load())
	}
}

func TestJournalPagesAndCursors(t *testing.T) {
	c, _ := openTest(t)
	for n := 2; n < 7; n++ {
		if _, err := c.Commit(context.Background(), c.Epoch(), proposal(n, fmt.Sprint(n), ""), success); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.Changes(1, 2)
	if err != nil || len(page) != 2 || page[0].Sequence != 2 || page[1].Sequence != 3 {
		t.Fatal(page, err)
	}
	if _, err := c.Changes(6, 2); err == nil {
		t.Fatal("future cursor accepted")
	}
	if _, err := c.Changes(0, MaxPage+1); err == nil {
		t.Fatal("unbounded page accepted")
	}
	if err := c.Acknowledge(id(1), 0, 2); err == nil {
		t.Fatal("cursor skipped a batch")
	}
	if err := c.Acknowledge(id(1), 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Acknowledge(id(1), 0, 1); err != nil {
		t.Fatal("idempotent acknowledgement failed", err)
	}
	if err := c.Acknowledge(id(1), 2, 3); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if got, err := c.Cursor(id(1)); err != nil || got != 1 {
		t.Fatal(got, err)
	}
	if got, err := c.Cursor(id(2)); err != nil || got != 0 {
		t.Fatal("cursor leaked between clients", got, err)
	}
	if err := c.Acknowledge(id(1), 1, 2); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidProposalAndCancellation(t *testing.T) {
	c, _ := openTest(t)
	for _, path := range []string{"", ".", "../outside", "a/../b", "a\\b", ".nas-sync/file", "a/.nas-sync/b"} {
		if _, err := c.Commit(context.Background(), c.Epoch(), proposal(2, path, ""), success); err == nil {
			t.Fatal("accepted invalid path", path)
		}
	}
	p := proposal(2, "a", "")
	p.Entries = append(p.Entries, proposal(3, "a/b", "").Entries...)
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err == nil {
		t.Fatal("accepted overlapping paths")
	}
	p = proposal(2, "a", "")
	second := p.Entries[0]
	second.Path = "b"
	p.Entries = append(p.Entries, second)
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); !errors.Is(err, ErrIdentity) {
		t.Fatal("accepted duplicate version within batch", err)
	}
	p = proposal(2, "a", "")
	p.Entries[0].Next.Tombstone = true
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err == nil {
		t.Fatal("accepted tombstone payload")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Commit(ctx, c.Epoch(), proposal(2, "a", ""), success); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, pending, err := c.Head("a"); pending || err != nil {
		t.Fatal("canceled request prepared", pending, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	if _, err := c.Commit(ctx, c.Epoch(), proposal(2, "a", ""), publishFunc(func(context.Context, Record) error { cancel(); return nil })); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, pending, err := c.Head("a"); !pending || err != nil {
		t.Fatal("canceled publication lost recovery", pending, err)
	}
	if _, err := c.Recover(context.Background(), c.Epoch(), success); err != nil {
		t.Fatal(err)
	}
}

func TestKilledPublisherAndExclusiveOwnership(t *testing.T) {
	if dir := os.Getenv("ANANAS_JOURNAL_CRASH_TEST"); dir != "" {
		c, err := Open(dir, id(99))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Commit(context.Background(), c.Epoch(), proposal(2, "file", ""), publishFunc(func(context.Context, Record) error {
			fmt.Fprintln(os.Stdout, "prepared")
			for {
				time.Sleep(time.Hour)
			}
		}))
		t.Fatal("publisher unexpectedly returned", err)
	}
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKilledPublisherAndExclusiveOwnership$")
	cmd.Env = append(os.Environ(), "ANANAS_JOURNAL_CRASH_TEST="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "prepared\n" {
		t.Fatal(line, err)
	}
	if competing, err := Open(dir, id(99)); err == nil {
		competing.Close()
		t.Fatal("second process acquired live coordinator's journal")
	}
	cmd.Process.Kill()
	cmd.Wait()
	c, err := Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Epoch() != 2 {
		t.Fatal("unexpected recovered epoch", c.Epoch())
	}
	if head, pending, err := c.Head("file"); err != nil || head != nil || !pending {
		t.Fatal("killed publisher lost prepared state", head, pending, err)
	}
	if _, err := c.Recover(context.Background(), c.Epoch(), success); err != nil {
		t.Fatal(err)
	}
	if page, err := c.Changes(0, 1); err != nil || len(page) != 1 || page[0].Sequence != 1 {
		t.Fatal(page, err)
	}
}

func TestNamespaceAndJournalSymlink(t *testing.T) {
	c, dir := openTest(t)
	c.Close()
	if other, err := Open(dir, id(100)); err == nil {
		other.Close()
		t.Fatal("opened wrong namespace")
	}
	dir = filepath.Join(t.TempDir(), "store")
	os.Mkdir(dir, 0700)
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("untouched"), 0600)
	if err := os.Symlink(outside, filepath.Join(dir, "journal.db")); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(dir, id(99)); err == nil {
		other.Close()
		t.Fatal("followed journal symlink")
	}
	if got, err := os.ReadFile(outside); err != nil || strings.TrimSpace(string(got)) != "untouched" {
		t.Fatal("modified outside file", err)
	}
}

func TestTombstoneAndPersistentCursor(t *testing.T) {
	c, dir := openTest(t)
	p := proposal(2, "file", "")
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil {
		t.Fatal(err)
	}
	deleted := proposal(3, "file", p.Entries[0].Next.ID)
	deleted.Entries[0].Next = Version{ID: id(1003), Tombstone: true}
	if _, err := c.Commit(context.Background(), c.Epoch(), deleted, success); err != nil {
		t.Fatal(err)
	}
	if err := c.Acknowledge(id(1), 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Acknowledge(id(1), 1, 2); err != nil {
		t.Fatal(err)
	}
	c.Close()
	reopened, err := Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if cursor, err := reopened.Cursor(id(1)); err != nil || cursor != 2 {
		t.Fatal(cursor, err)
	}
	if head, pending, err := reopened.Head("file"); err != nil || pending || head == nil || !head.Tombstone {
		t.Fatal(head, pending, err)
	}
	// Missing expected head is different from an acknowledged tombstone.
	if _, err := reopened.Commit(context.Background(), reopened.Epoch(), proposal(4, "file", ""), success); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := reopened.Commit(context.Background(), reopened.Epoch(), proposal(4, "file", deleted.Entries[0].Next.ID), success); err != nil {
		t.Fatal(err)
	}
}

func TestJournalGapFailsClosed(t *testing.T) {
	c, _ := openTest(t)
	if _, err := c.Commit(context.Background(), c.Epoch(), proposal(2, "file", ""), success); err != nil {
		t.Fatal(err)
	}
	if err := c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(changesBucket).Delete(key(1)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Changes(0, 1); err == nil {
		t.Fatal("missing journal batch treated as caught up")
	}
	if err := c.Acknowledge(id(1), 0, 1); err == nil {
		t.Fatal("acknowledged missing batch")
	}
}

func TestBatchBounds(t *testing.T) {
	c, _ := openTest(t)
	p := proposal(2, "file", "")
	for n := 1; n < MaxEntries; n++ {
		p.Entries = append(p.Entries, proposal(n+2, fmt.Sprint(n), "").Entries[0])
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil {
		t.Fatal("maximum bounded batch failed", err)
	}
	p.ID = id(500)
	p.Entries = append(p.Entries, proposal(600, "extra", "").Entries[0])
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err == nil {
		t.Fatal("accepted oversized entry count")
	}
	p = proposal(700, "file", "")
	for n := 0; n < 40; n++ {
		p.Entries = append(p.Entries, proposal(n+701, fmt.Sprint(n)+strings.Repeat("x", 4000), "").Entries[0])
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err == nil {
		t.Fatal("accepted oversized encoded batch")
	}
}

func TestDirectoryVersionValidation(t *testing.T) {
	c, _ := openTest(t)
	for _, v := range []Version{
		{ID: id(2), Directory: true, Size: 1},
		{ID: id(2), Directory: true, Digest: hash.SumBytes(nil)},
		{ID: id(2), Directory: true, Tombstone: true},
	} {
		p := proposal(2, "folder", "")
		p.Entries[0].Next = v
		if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err == nil {
			t.Fatal("invalid directory accepted", v)
		}
	}
	p := proposal(2, "folder", "")
	p.Entries[0].Next = Version{ID: id(2), Directory: true}
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Version(id(2)); err != nil || !v.Directory {
		t.Fatal(v, err)
	}
}
