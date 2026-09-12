package replica

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func preparationFixture(t *testing.T) (*fixture, *index.DB, *content.Root) {
	t.Helper()
	f := setup(t)
	db, err := index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	root, err := content.OpenRoot(f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return f, db, root
}

func TestPrepareUploadBatchesFileAndDirectory(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	observeFile(t, db, f.root, "file", []byte("new file"))
	if err := os.Mkdir(filepath.Join(f.root, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(f.root, "folder"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "folder", Fingerprint: content.Fingerprint(st)}}, true); err != nil {
		t.Fatal(err)
	}
	u, decisions, err := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"file", "folder"}, id("remote namespace"), completionOptions())
	if err != nil || u == nil || len(u.Proposal.Entries) != 2 {
		t.Fatal(u, decisions, err)
	}
	for _, d := range decisions {
		if d.Decision.Action != diff.Push {
			t.Fatal(d)
		}
	}
	if !u.Proposal.Entries[1].Next.Directory || u.DeltaBytes[1] != 0 {
		t.Fatal("directory encoded file data", u)
	}
	if _, err := f.store.OpenReady(u.Proposal.Entries[1].Next.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("directory snapshot exists", err)
	}
	if pending, err := db.PendingUploadPreparation(); err != nil || pending != nil {
		t.Fatal(pending, err)
	}
	// Prepared data is owned by the outbox. Abort cannot remove it.
	if err := AbortUploadPreparation(context.Background(), db, f.c, f.store, u.Namespace); err != nil {
		t.Fatal(err)
	}
	file, err := f.store.OpenReady(u.Proposal.Entries[0].Next.ID)
	if err != nil {
		t.Fatal("abort removed outbox snapshot", err)
	}
	file.Close()
}

func TestPrepareUploadFailureRetainsProvenanceAndNewerEdit(t *testing.T) {
	f, db, root := preparationFixture(t)
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}, id("remote namespace")); err != nil {
		t.Fatal(err)
	}
	observeFile(t, db, f.root, "file", []byte("original"))
	o := completionOptions()
	changed := false
	o.Gate = func(context.Context) error {
		p, err := db.PendingUploadPreparation()
		if err != nil {
			return err
		}
		if p != nil && !changed {
			file, err := f.store.OpenReady(p.Sources[0].ID)
			if err == nil {
				file.Close()
				changed = true
				observeFile(t, db, f.root, "file", []byte("newer local edit"))
			}
		}
		return nil
	}
	if _, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"file"}, id("remote namespace"), o); !errors.Is(err, index.ErrStale) || !changed {
		t.Fatal("changed snapshot accepted", err)
	}
	p, err := db.PendingUploadPreparation()
	if err != nil || p == nil {
		t.Fatal("failed capture lost ownership", p, err)
	}
	if u, err := db.PendingUpload(); err != nil || u != nil {
		t.Fatal("failed preparation became sendable", u, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"file"}, p.Namespace, completionOptions()); !errors.Is(err, index.ErrStale) {
		t.Fatal("abandoned preparation did not block new work", err)
	}
	if err := AbortUploadPreparation(context.Background(), db, f.c, f.store, id("wrong namespace")); err == nil {
		t.Fatal("wrong namespace accepted for cleanup")
	}
	if err := AbortUploadPreparation(context.Background(), db, f.c, f.store, p.Namespace); err != nil {
		t.Fatal(err)
	}
	if u, err := f.c.PublicationBudgetUsage(); err != nil || u == nil || u.ReservedBytes != 0 || u.Entries != 0 {
		t.Fatal("abort did not release failed snapshot charge", u, err)
	}
	if err := f.store.RequireUploadVacant(p.Sources[0].ID); err != nil {
		t.Fatal("owned artifacts remain", err)
	}
	if err := AbortUploadPreparation(context.Background(), db, f.c, f.store, p.Namespace); err != nil {
		t.Fatal("repeated abort failed", err)
	}
	r, ok, err := db.Get("file")
	if err != nil || !ok || !r.Dirty || r.Generation <= p.Sources[0].Generation {
		t.Fatal("newer work was lost", r, err)
	}
	u, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"file"}, p.Namespace, completionOptions())
	if err != nil || u == nil || u.Generations[0] != r.Generation {
		t.Fatal("fresh preparation failed", u, err)
	}
}

func TestPrepareUploadRejectsPolicyBeforeCapture(t *testing.T) {
	for _, scenario := range []string{"disabled", "paused", "excluded", "batch limit", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			f, db, root := preparationFixture(t)
			defer db.Close()
			observeFile(t, db, f.root, "file", []byte("some bytes"))
			o := completionOptions()
			paths := []string{"file"}
			switch scenario {
			case "disabled":
				o.Writes = false
			case "paused":
				if err := db.SetPaused(true); err != nil {
					t.Fatal(err)
				}
			case "excluded":
				o.Exclusions = []string{"file"}
			case "batch limit":
				o.MaxFileBytes, o.MaxBatchBytes = 1, 1
			case "duplicate":
				paths = append(paths, "file")
			}
			remote := &comparisonRemote{onHead: func() { t.Fatal("rejected preparation contacted remote") }}
			if _, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, remote, paths, id("remote namespace"), o); err == nil {
				t.Fatal("invalid preparation accepted")
			}
			if p, err := db.PendingUploadPreparation(); err != nil || p != nil {
				t.Fatal("rejected preparation claimed storage", p, err)
			}
		})
	}
}

func TestPrepareUploadConflictDoesNotCreateOutbox(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	observeFile(t, db, f.root, "file", []byte("local independent file"))
	data := []byte("remote independent file")
	v := journal.Version{ID: id("remote existing"), Size: int64(len(data)), Digest: hash.SumBytes(data)}
	remote := &comparisonRemote{v: &v, data: data}
	u, decisions, err := PrepareUpload(context.Background(), db, root, f.c, f.store, remote, []string{"file"}, id("remote namespace"), completionOptions())
	if err != nil || u != nil || len(decisions) != 1 || decisions[0].Decision.Action != diff.Conflict {
		t.Fatal("conflict prepared for upload", u, decisions, err)
	}
	if p, err := db.PendingUploadPreparation(); err != nil || p != nil {
		t.Fatal("conflict claimed candidates", p, err)
	}
	if dirty, err := db.DirtyPage("", 1); err != nil || len(dirty) != 1 {
		t.Fatal("conflict lost source work", dirty, err)
	}
	if err := f.store.RequireUploadVacant(decisions[0].Local.Content.ID); err != nil {
		t.Fatal("conflict created snapshot", err)
	}
}
