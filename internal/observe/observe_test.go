package observe

import (
	"context"
	"nas-sync/internal/config"
	"nas-sync/internal/index"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setup(t *testing.T) (*config.Config, *index.DB) {
	t.Helper()
	cfg := config.Default()
	cfg.Local.Root = t.TempDir()
	cfg.NAS.MountPoint = "/mnt/test-nas-unmounted"
	cfg.Limits.ScanOpsPerSecond = 10000
	cfg.Coalesce.Idle = config.Duration(10 * time.Millisecond)
	cfg.Coalesce.MaxWait = config.Duration(40 * time.Millisecond)
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"), cfg.Local.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return cfg, db
}
func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
}
func snapshot(t *testing.T, cfg *config.Config, db *index.DB) {
	t.Helper()
	o, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = o.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func wait(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
func TestSnapshotRestartExclusionsAndDeletion(t *testing.T) {
	cfg, db := setup(t)
	cfg.Selective.ExcludeLocal = []string{"ignored/"}
	cfg.Selective.ExcludeRemote = []string{"remote/"}
	for _, d := range []string{"kept", "ignored", "remote"} {
		if err := os.Mkdir(filepath.Join(cfg.Local.Root, d), 0700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(cfg.Local.Root, d, "a"), "content")
	}
	snapshot(t, cfg, db)
	for _, p := range []string{"ignored/a", "remote/a"} {
		if _, ok, _ := db.Get(p); ok {
			t.Fatalf("excluded %s tracked", p)
		}
	}
	before, ok, err := db.Get("kept/a")
	if err != nil || !ok {
		t.Fatal(err)
	}
	snapshot(t, cfg, db)
	after, _, _ := db.Get("kept/a")
	if before.Generation != after.Generation {
		t.Fatal("unchanged restart dirtied file")
	}
	if err := os.Remove(filepath.Join(cfg.Local.Root, "kept", "a")); err != nil {
		t.Fatal(err)
	}
	snapshot(t, cfg, db)
	after, _, _ = db.Get("kept/a")
	if !after.Missing || after.Generation <= before.Generation {
		t.Fatal(after)
	}
}
func TestWatcherReplacementMoveAndIdle(t *testing.T) {
	cfg, db := setup(t)
	write(t, filepath.Join(cfg.Local.Root, "file"), "old")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("observer did not stop")
		}
	})
	wait(t, func() bool { return o.Status().Ready })
	old, _, _ := db.Get("file")
	write(t, filepath.Join(cfg.Local.Root, "tmp"), "replacement")
	if err = os.Rename(filepath.Join(cfg.Local.Root, "tmp"), filepath.Join(cfg.Local.Root, "file")); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		r, _, _ := db.Get("file")
		return r.Generation > old.Generation && r.Fingerprint.Size == 11
	})
	dir := filepath.Join(cfg.Local.Root, "new")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "child"), "a")
	wait(t, func() bool { r, ok, _ := db.Get("new/child"); return ok && !r.Missing })
	if err = os.Rename(dir, filepath.Join(cfg.Local.Root, "moved")); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		r, ok, _ := db.Get("moved/child")
		old, _, _ := db.Get("new/child")
		return ok && !r.Missing && old.Missing
	})
	wait(t, func() bool { return !o.Status().Scanning })
	time.Sleep(100 * time.Millisecond)
	before := o.Status()
	time.Sleep(100 * time.Millisecond)
	after := o.Status()
	if before.MetadataReads != after.MetadataReads {
		t.Fatalf("idle metadata work: %d -> %d", before.MetadataReads, after.MetadataReads)
	}
}
func TestIncompleteScanDoesNotInferDeletion(t *testing.T) {
	cfg, db := setup(t)
	write(t, filepath.Join(cfg.Local.Root, "keep"), "content")
	snapshot(t, cfg, db)
	if err := os.Remove(filepath.Join(cfg.Local.Root, "keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.Local.Root, "newdir"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Limits.MaxWatches = 1
	o, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = o.ScanOnce(context.Background()); err == nil {
		t.Fatal("watch limit did not stop incomplete scan")
	}
	r, _, _ := db.Get("keep")
	if r.Missing {
		t.Fatal("incomplete scan inferred deletion")
	}
	required, _ := db.RecoveryRequired()
	if !required {
		t.Fatal("recovery marker cleared")
	}
}
func TestSymlinkCannotEscapeRoot(t *testing.T) {
	cfg, db := setup(t)
	external := t.TempDir()
	write(t, filepath.Join(external, "secret"), "do not read")
	if err := os.Symlink(external, filepath.Join(cfg.Local.Root, "link")); err != nil {
		t.Fatal(err)
	}
	snapshot(t, cfg, db)
	r, ok, _ := db.Get("link")
	if !ok || !r.Excluded {
		t.Fatal("symlink not reported")
	}
	if _, ok, _ := db.Get("link/secret"); ok {
		t.Fatal("traversed external symlink")
	}
}
func TestBurstCoalescesAndPreservesFinalState(t *testing.T) {
	cfg, db := setup(t)
	cfg.Coalesce.Idle = config.Duration(100 * time.Millisecond)
	cfg.Coalesce.MaxWait = config.Duration(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	defer func() { cancel(); <-done }()
	wait(t, func() bool { return o.Status().Ready })
	for i := 0; i < 500; i++ {
		write(t, filepath.Join(cfg.Local.Root, "hot"), "rewrite")
	}
	write(t, filepath.Join(cfg.Local.Root, "hot"), "final")
	wait(t, func() bool { r, ok, _ := db.Get("hot"); return ok && r.Fingerprint.Size == 5 })
	if o.Status().Batches > 3 {
		t.Fatalf("too many batches: %+v", o.Status())
	}
}

func TestDirectoryReplacedBySymlinkDoesNotDeleteChildren(t *testing.T) {
	cfg, db := setup(t)
	dir := filepath.Join(cfg.Local.Root, "dir")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "a"), "keep")
	snapshot(t, cfg, db)
	if err := os.Rename(dir, filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	snapshot(t, cfg, db)
	r, _, _ := db.Get("dir/a")
	if r.Missing || !r.Excluded {
		t.Fatalf("unsafe descendant state: %+v", r)
	}
}

func TestSingleFileEventDoesNotScanSiblings(t *testing.T) {
	cfg, db := setup(t)
	for _, p := range []string{"a", "b", "c"} {
		write(t, filepath.Join(cfg.Local.Root, p), "content")
	}
	o, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	defer o.root.Close()
	defer o.watcher.Close()
	if err = o.scan(context.Background(), ".", false); err != nil {
		t.Fatal(err)
	}
	before := o.Status().MetadataReads
	write(t, filepath.Join(cfg.Local.Root, "b"), "changed")
	if err = o.scan(context.Background(), "b", true); err != nil {
		t.Fatal(err)
	}
	if o.Status().MetadataReads-before != 1 {
		t.Fatal("file change rescanned siblings")
	}
	b, _, _ := db.Get("b")
	a, _, _ := db.Get("a")
	if b.Generation != 2 || a.Generation != 1 {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
}

func TestUnavailableRootCannotDeleteIndexedFiles(t *testing.T) {
	cfg, db := setup(t)
	write(t, filepath.Join(cfg.Local.Root, "keep"), "content")
	snapshot(t, cfg, db)
	o, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(cfg.Local.Root, filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatal(err)
	}
	if err = o.ScanOnce(context.Background()); err == nil {
		t.Fatal("missing root was accepted")
	}
	r, _, _ := db.Get("keep")
	if r.Missing {
		t.Fatal("unavailable root caused deletion")
	}
}

func TestReconfirmedAbsenceRefreshesMissingGeneration(t *testing.T) {
	cfg, db := setup(t)
	name := filepath.Join(cfg.Local.Root, "file")
	write(t, name, "original")
	o, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	defer o.root.Close()
	defer o.watcher.Close()
	if err := o.scan(context.Background(), ".", false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := o.scan(context.Background(), "file", true); err != nil {
		t.Fatal(err)
	}
	before, _, _ := db.Get("file")
	if ok, err := db.ClearObserved(before); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// Another writer can publish and remove a file within one coalescing
	// window (or while the observer is stopped). A prior acknowledged absence
	// must not hide the new deletion from the transfer worker.
	for _, force := range []bool{true, false} {
		write(t, name, "intervening version")
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
		root := "file"
		if !force {
			root = "." // restart scan
		}
		if err := o.scan(context.Background(), root, force); err != nil {
			t.Fatal(err)
		}
		after, _, _ := db.Get("file")
		if !after.Missing || !after.Dirty || after.Generation <= before.Generation {
			t.Fatal("coalesced deletion lost", before, after)
		}
		if ok, err := db.ClearObserved(after); err != nil || !ok {
			t.Fatal(ok, err)
		}
		before = after
	}
}
