package transferapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nas-sync/internal/config"
	"nas-sync/internal/content"
	"nas-sync/internal/daemon"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/observe"
	"nas-sync/internal/publish"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

func TestTLSWorkerDirectoryRenameConverges(t *testing.T) {
	dir := t.TempDir()
	rootPath, state := filepath.Join(dir, "root"), filepath.Join(dir, "state")
	for _, p := range []string{rootPath, state} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := index.Open(filepath.Join(state, "index.db"), rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clientID, err := db.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	server := setupWithClientID(t, true, nil, clientID)
	client := newTestClient(t, clientOptions(server))
	c, err := journal.Open(state, identifier("worker replica"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 256 << 20, MaxEntries: 128}, server.c.Namespace()); err != nil {
		t.Fatal(err)
	}
	store, err := stage.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root, err := content.OpenRoot(rootPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publisher, err := publish.Open(rootPath, state, c.Version, nil, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	cfg := config.Default()
	cfg.Local.Root, cfg.StateDir, cfg.NAS.MountPoint = rootPath, state, "/mnt/unused-worker-test"
	cfg.Limits.ScanOpsPerSecond = 10000
	cfg.Coalesce.Idle, cfg.Coalesce.MaxWait = config.Duration(20*time.Millisecond), config.Duration(60*time.Millisecond)
	observer, err := observe.New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := replica.NewWorker(db, root, c, store, publisher, client, replica.WorkerOptions{Namespace: server.c.Namespace(), PushOptions: pushOptions(), Ready: func() bool { s := observer.Status(); return s.Ready && !s.Scanning }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- daemon.RunWithRemote(ctx, observer, nil, worker, daemon.Surface{}, client) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	workerWait(t, worker, func() bool { return observer.Status().Ready && worker.Status().Phase == "idle" })
	if err := os.MkdirAll(filepath.Join(rootPath, "old/inner/empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "old/inner/file"), []byte("keep contents"), 0600); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		b, err := os.ReadFile(filepath.Join(server.root, "old/inner/file"))
		return err == nil && string(b) == "keep contents" && worker.Status().Phase == "idle"
	})
	// Repeat in both lexical directions, with nested files and empty directories.
	for _, names := range [][2]string{{"old", "renamed"}, {"renamed", "aaa"}} {
		if err := os.Rename(filepath.Join(rootPath, names[0]), filepath.Join(rootPath, names[1])); err != nil {
			t.Fatal(err)
		}
		workerWait(t, worker, func() bool {
			b, err := os.ReadFile(filepath.Join(server.root, names[1], "inner/file"))
			_, oldErr := os.Stat(filepath.Join(server.root, names[0]))
			empty, emptyErr := os.Stat(filepath.Join(server.root, names[1], "inner/empty"))
			dirty, dbErr := db.DirtyPage("", 32)
			return err == nil && string(b) == "keep contents" && os.IsNotExist(oldErr) && emptyErr == nil && empty.IsDir() && dbErr == nil && len(dirty) == 0 && worker.Status().Phase == "idle"
		})
	}
	// A directory originally uploaded by this PC must also support later NAS
	// deletion. Logical base adoption alone is not a native identity receipt.
	for _, path := range []string{"aaa/inner/file", "aaa/inner/empty", "aaa/inner", "aaa"} {
		base, pending, err := server.c.Head(path)
		if err != nil || pending || base == nil {
			t.Fatal(base, pending, err)
		}
		p := journal.Proposal{ID: identifier("NAS delete " + path), Client: identifier("other writer"), Entries: []journal.Entry{{Path: path, Expected: base.ID, Next: journal.Version{ID: identifier("deleted " + path), Tombstone: true}}}}
		if _, err := server.c.Commit(ctx, server.c.Epoch(), p, server.publisher); err != nil {
			t.Fatal(err)
		}
	}
	worker.Notify()
	workerWait(t, worker, func() bool {
		_, err := os.Stat(filepath.Join(rootPath, "aaa"))
		dirty, dbErr := db.DirtyPage("", 32)
		return os.IsNotExist(err) && dbErr == nil && len(dirty) == 0 && worker.Status().Phase == "idle"
	})
}
