package transferapi

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/publish"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

type auditRemote struct {
	*Client
	mutate      func(string)
	unavailable atomic.Bool
}

func (r *auditRemote) CompareHead(ctx context.Context, namespace, path string) (*journal.Version, bool, error) {
	if r.unavailable.Swap(false) {
		return nil, false, &RemoteError{Status: 503, Message: "temporary fixture outage"}
	}
	v, p, e := r.Client.CompareHead(ctx, namespace, path)
	if r.mutate != nil {
		r.mutate(path)
	}
	return v, p, e
}

func TestTLSWorkerIsolatesBadSourcesAndRetriesChangedFiles(t *testing.T) {
	for _, kind := range []string{"unsupported", "permission", "changed", "replaced by symlink", "temporary NAS outage"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "permission" && os.Geteuid() == 0 {
				t.Skip("permission test needs an ordinary user")
			}
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
			remote := &auditRemote{Client: newTestClient(t, clientOptions(server))}
			c, err := journal.Open(state, identifier("audit replica"))
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
			add := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(rootPath, path), data, 0600); err != nil {
					t.Fatal(err)
				}
				st, err := os.Lstat(filepath.Join(rootPath, path))
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Put([]index.Record{{Path: path, Fingerprint: content.Fingerprint(st)}}, false); err != nil {
					t.Fatal(err)
				}
			}
			add("a-good", []byte("first"))
			add("z-good", []byte("last"))
			switch kind {
			case "unsupported":
				add(`m-\`, []byte("unsupported name"))
				add("n-big", make([]byte, 129))
				if err := os.Symlink("a-good", filepath.Join(rootPath, "o-link")); err != nil {
					t.Fatal(err)
				}
				st, err := os.Lstat(filepath.Join(rootPath, "o-link"))
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Put([]index.Record{{Path: "o-link", Fingerprint: content.Fingerprint(st)}}, false); err != nil {
					t.Fatal(err)
				}
			case "permission":
				add("m-denied", []byte("private"))
				if err := os.Chmod(filepath.Join(rootPath, "m-denied"), 0); err != nil {
					t.Fatal(err)
				}
				// Index the current metadata, so the failure really is EACCES.
				st, err := os.Stat(filepath.Join(rootPath, "m-denied"))
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Put([]index.Record{{Path: "m-denied", Fingerprint: content.Fingerprint(st)}}, false); err != nil {
					t.Fatal(err)
				}
			case "changed", "replaced by symlink":
				add("m-changing", []byte("before"))
				var changed atomic.Bool
				remote.mutate = func(path string) {
					if path == "m-changing" && !changed.Swap(true) {
						if kind == "replaced by symlink" {
							if err := os.Remove(filepath.Join(rootPath, path)); err != nil {
								panic(err)
							}
							if err := os.Symlink("a-good", filepath.Join(rootPath, path)); err != nil {
								panic(err)
							}
							return
						}
						if err := os.WriteFile(filepath.Join(rootPath, path), []byte("after"), 0600); err != nil {
							panic(err)
						}
					}
				}
			case "temporary NAS outage":
				remote.unavailable.Store(true)
			}
			opts := pushOptions()
			opts.MaxFileBytes, opts.MaxBatchBytes = 128, 256
			worker, err := replica.NewWorker(db, root, c, store, publisher, remote, replica.WorkerOptions{Namespace: server.c.Namespace(), PushOptions: opts, Ready: func() bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- worker.Run(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			workerWait(t, worker, func() bool { a, _, _ := db.Get("a-good"); z, _, _ := db.Get("z-good"); return !a.Dirty && !z.Dirty })
			for _, name := range []string{"a-good", "z-good"} {
				if _, err := os.Stat(filepath.Join(server.root, name)); err != nil {
					t.Fatal("healthy file starved", name, err)
				}
			}
			if kind == "unsupported" || kind == "permission" || kind == "replaced by symlink" {
				workerWait(t, worker, func() bool { return worker.Status().Phase == "partial" })
				pending, err := db.PendingSync(ctx, "", 128)
				if err != nil || pending.Total == 0 {
					t.Fatal(pending, err)
				}
			}
			if kind == "permission" {
				if err := os.Chmod(filepath.Join(rootPath, "m-denied"), 0600); err != nil {
					t.Fatal(err)
				}
				r, _, err := db.Get("m-denied")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ConfirmSync(ctx, index.SyncConfirmation{Path: r.Path, Generation: r.Generation}, 128); err != nil {
					t.Fatal(err)
				}
				worker.RequestSync()
				workerWait(t, worker, func() bool { r, _, _ := db.Get("m-denied"); return !r.Dirty })
				b, err := os.ReadFile(filepath.Join(server.root, "m-denied"))
				if err != nil || string(b) != "private" {
					t.Fatal(string(b), err)
				}
			}
			if kind == "changed" {
				workerWait(t, worker, func() bool { r, _, _ := db.Get("m-changing"); return !r.Dirty })
				b, err := os.ReadFile(filepath.Join(server.root, "m-changing"))
				if err != nil || string(b) != "after" {
					t.Fatal(string(b), err)
				}
			}
		})
	}
}
