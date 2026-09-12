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
	"nas-sync/internal/nasobserve"
	"nas-sync/internal/observe"
	"nas-sync/internal/publish"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

func TestTLSNativeDirectoryObservationReachesReplica(t *testing.T) {
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
	patterns := []string{"skip/"}
	server := setupWithClientID(t, true, patterns, clientID)
	if err := server.c.ConfigurePublicationBudget(journal.PublicationBudget{MaxBytes: 256 << 20, MaxEntries: 128}); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, clientOptions(server))
	c, err := journal.Open(state, identifier("native directory replica"))
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
	root, err := content.OpenRoot(rootPath, patterns)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publisher, err := publish.Open(rootPath, state, c.Version, patterns, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	cfg := config.Default()
	cfg.Local.Root, cfg.StateDir, cfg.NAS.MountPoint = rootPath, state, "/mnt/unused-native-directory-test"
	cfg.Selective.ExcludeLocal = patterns
	cfg.Limits.ScanOpsPerSecond = 10000
	cfg.Coalesce.Idle, cfg.Coalesce.MaxWait = config.Duration(20*time.Millisecond), config.Duration(60*time.Millisecond)
	observer, err := observe.New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	opts := pushOptions()
	opts.Exclusions = patterns
	worker, err := replica.NewWorker(db, root, c, store, publisher, client, replica.WorkerOptions{Namespace: server.c.Namespace(), PushOptions: opts, Ready: func() bool { s := observer.Status(); return s.Ready && !s.Scanning }})
	if err != nil {
		t.Fatal(err)
	}
	nativeDB, err := index.Open(filepath.Join(t.TempDir(), "index.db"), server.root)
	if err != nil {
		t.Fatal(err)
	}
	defer nativeDB.Close()
	nativeObserver, err := observe.NewNative(server.root, patterns, nativeDB)
	if err != nil {
		t.Fatal(err)
	}
	nativeWorker, err := nasobserve.New(nativeDB, server.c, server.publisher, server.store, 1<<20, patterns, func() bool { s := nativeObserver.Status(); return s.Ready && !s.Scanning })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pcDone, nasDone := make(chan error, 1), make(chan error, 1)
	go func() { pcDone <- daemon.RunWithRemote(ctx, observer, nil, worker, daemon.Surface{}, client) }()
	go func() { nasDone <- daemon.Run(ctx, nativeObserver, nil, nativeWorker, daemon.Surface{}) }()
	defer func() {
		if t.Failed() {
			page, _ := db.DirtyPage("", 32)
			for _, r := range page {
				fp, missing, err := root.Metadata(r.Path)
				t.Log("PC dirty", r, "actual", fp, missing, err)
			}
			t.Log("observers", observer.Status(), nativeObserver.Status(), "native worker", nativeWorker.Status())
		}
		cancel()
		for _, ch := range []chan error{pcDone, nasDone} {
			select {
			case err := <-ch:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("native/PC lifecycle did not join")
			}
		}
	}()
	workerWait(t, worker, func() bool { return observer.Status().Ready && nativeObserver.Status().Ready })
	for _, p := range []string{"from-nas/inner", "from-nas/empty", "from-nas/skip"} {
		if err := os.MkdirAll(filepath.Join(server.root, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for p, data := range map[string]string{"from-nas/inner/file": "created on NAS", "from-nas/skip/private": "excluded"} {
		if err := os.WriteFile(filepath.Join(server.root, p), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	workerWait(t, worker, func() bool {
		data, err := os.ReadFile(filepath.Join(rootPath, "from-nas/inner/file"))
		empty, e := os.Stat(filepath.Join(rootPath, "from-nas/empty"))
		return err == nil && string(data) == "created on NAS" && e == nil && empty.IsDir() && worker.Status().Phase == "idle"
	})
	records, err := server.c.Changes(0, 32)
	if err != nil || len(records) != 4 {
		t.Fatal("unexpected directory journal", records, err, nativeWorker.Status())
	}
	for i, path := range []string{"from-nas", "from-nas/empty", "from-nas/inner", "from-nas/inner/file"} {
		if records[i].Entries[0].Path != path {
			t.Fatal("child preceded its directory", records)
		}
	}
	if _, err := os.Stat(filepath.Join(rootPath, "from-nas/skip")); !os.IsNotExist(err) {
		t.Fatal("excluded directory reached PC", err)
	}
	if _, ok, err := nativeDB.Get("from-nas/skip/private"); err != nil || ok {
		t.Fatal("excluded file was observed", ok, err)
	}
	// A subsequent PC write can use the NAS-imported immutable base normally.
	if err := os.WriteFile(filepath.Join(rootPath, "from-nas/inner/file"), []byte("edited on PC"), 0600); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		b, e := os.ReadFile(filepath.Join(server.root, "from-nas/inner/file"))
		return e == nil && string(b) == "edited on PC" && worker.Status().Phase == "idle"
	})
	seq, _, err := server.c.Observe()
	if err != nil {
		t.Fatal(err)
	}
	batch := nativeObserver.Status().Batches
	if err := os.Chtimes(filepath.Join(server.root, "from-nas/inner"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		p, e := nativeDB.DirtyPage("", 32)
		return e == nil && len(p) == 0 && nativeObserver.Status().Batches > batch
	})
	after, _, err := server.c.Observe()
	if err != nil || after != seq {
		t.Fatal("directory child/metadata event created a feedback commit", seq, after, err)
	}
	// A native unlink of a file is imported without rewriting the NAS path,
	// reaches the replica, and leaves the containing directories intact.
	if err := os.Remove(filepath.Join(server.root, "from-nas/inner/file")); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		_, err := os.Lstat(filepath.Join(rootPath, "from-nas/inner/file"))
		base, e := db.Base("from-nas/inner/file")
		return os.IsNotExist(err) && e == nil && base != nil && base.Content.Tombstone && worker.Status().Phase == "idle"
	})
	if info, err := os.Stat(filepath.Join(rootPath, "from-nas/inner")); err != nil || !info.IsDir() {
		t.Fatal("file deletion altered its parent", info, err)
	}
	// Re-creation uses the tombstone as its base; a following PC deletion must
	// not trigger a feedback import on the NAS observer.
	if err := os.WriteFile(filepath.Join(rootPath, "from-nas/inner/file"), []byte("recreated on PC"), 0600); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		data, err := os.ReadFile(filepath.Join(server.root, "from-nas/inner/file"))
		return err == nil && string(data) == "recreated on PC" && worker.Status().Phase == "idle"
	})
	if err := os.Remove(filepath.Join(rootPath, "from-nas/inner/file")); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		_, err := os.Lstat(filepath.Join(server.root, "from-nas/inner/file"))
		page, e := nativeDB.DirtyPage("", 32)
		return os.IsNotExist(err) && e == nil && len(page) == 0 && worker.Status().Phase == "idle"
	})
	after, _, err = server.c.Observe()
	if err != nil || after != seq+3 {
		t.Fatal("delete/recreate/delete should commit exactly three versions", seq, after, err)
	}
	// A NAS deletion must not discard a PC edit made while synchronization was
	// paused. Preserve the divergent file and surface attention on resume.
	if err := os.WriteFile(filepath.Join(rootPath, "from-nas/inner/file"), []byte("shared before conflict"), 0600); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		data, err := os.ReadFile(filepath.Join(server.root, "from-nas/inner/file"))
		return err == nil && string(data) == "shared before conflict" && worker.Status().Phase == "idle"
	})
	if err := db.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	worker.Interrupt()
	workerWait(t, worker, func() bool { return worker.Status().Phase == "suspended" })
	if err := os.WriteFile(filepath.Join(rootPath, "from-nas/inner/file"), []byte("keep this PC edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(server.root, "from-nas/inner/file")); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		head, pending, err := server.c.Head("from-nas/inner/file")
		return err == nil && !pending && head != nil && head.Tombstone
	})
	if err := db.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	worker.Notify()
	workerWait(t, worker, func() bool { return worker.Status().Phase == "attention" })
	if data, err := os.ReadFile(filepath.Join(rootPath, "from-nas/inner/file")); err != nil || string(data) != "keep this PC edit" {
		t.Fatal("NAS deletion lost divergent PC edit", string(data), err)
	}
	if _, err := os.Lstat(filepath.Join(server.root, "from-nas/inner/file")); !os.IsNotExist(err) {
		t.Fatal("conflict silently recreated the NAS file", err)
	}
}
