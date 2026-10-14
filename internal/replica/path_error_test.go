package replica

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func TestFailedSnapshotIsAbortedBeforeOtherWorkCanContinue(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	namespace := id("remote namespace")
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}, namespace); err != nil {
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
			ready, err := f.store.OpenReady(p.Sources[0].ID)
			if err == nil {
				ready.Close()
				changed = true
				observeFile(t, db, f.root, "file", []byte("new version"))
			}
		}
		return nil
	}
	_, _, failure := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"file"}, namespace, o)
	if !errors.Is(failure, index.ErrStale) || !changed {
		t.Fatal(failure)
	}
	w := &Worker{db: db, root: root, c: f.c, store: f.store, opts: WorkerOptions{Namespace: namespace}}
	pass := workerPass{}
	deferred, err := w.deferLocalFailure(context.Background(), failure, &pass)
	if err != nil || !deferred || pass.retry == nil {
		t.Fatal(deferred, err, pass)
	}
	if p, err := db.PendingUploadPreparation(); err != nil || p != nil {
		t.Fatal("failed preparation still owns batch", p, err)
	}
	if u, err := db.PendingUpload(); err != nil || u != nil {
		t.Fatal(u, err)
	}
	usage, err := f.c.PublicationBudgetUsage()
	if err != nil || usage.ReservedBytes != 0 || usage.Entries != 0 {
		t.Fatal(usage, err)
	}
	r, _, _ := db.Get("file")
	issue, err := db.SyncIssue(r)
	if err != nil || issue == nil || !issue.Retry || !r.Dirty {
		t.Fatal(issue, r, err)
	}
	b, err := os.ReadFile(filepath.Join(f.root, "file"))
	if err != nil || string(b) != "new version" {
		t.Fatal(string(b), err)
	}
}

func TestSelectionGrowthRetriesWithoutPreparingAnOversizedBatch(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	observeFile(t, db, f.root, "first", []byte("12345"))
	observeFile(t, db, f.root, "second", []byte("12345"))
	o := completionOptions()
	o.MaxFileBytes, o.MaxBatchBytes = 8, 8
	_, _, err := PrepareUpload(context.Background(), db, root, f.c, f.store, &comparisonRemote{}, []string{"first", "second"}, id("remote namespace"), o)
	if !errors.Is(err, index.ErrStale) || !retryableWorkerError(err) {
		t.Fatal("grown batch did not request reselection", err)
	}
	if p, err := db.PendingUploadPreparation(); err != nil || p != nil {
		t.Fatal("oversized batch was prepared", p, err)
	}
}

func TestGlobalFailuresAreNotQuarantinedAsFileProblems(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	observeFile(t, db, f.root, "file", []byte("keep"))
	w := &Worker{db: db, root: root, c: f.c, store: f.store, opts: WorkerOptions{Namespace: id("remote namespace")}}
	for _, failure := range []error{journal.ErrBudget, journal.ErrIdentity, syscall.ENOSPC, syscall.EIO, context.Canceled} {
		deferred, err := w.deferLocalFailure(context.Background(), &localPathError{"file", 1, failure}, &workerPass{})
		if deferred || err != nil {
			t.Fatal("global failure isolated", failure, deferred, err)
		}
	}
	if err := os.Rename(f.root, f.root+"-old"); err != nil {
		t.Fatal(err)
	}
	deferred, err := w.deferLocalFailure(context.Background(), &localPathError{"file", 1, os.ErrNotExist}, &workerPass{})
	if deferred || !errors.Is(err, ErrAttention) {
		t.Fatal("missing root treated as missing file", deferred, err)
	}
}

func TestDisappearingSourceRefreshesObservationAndRetries(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint("nested=", nested), func(t *testing.T) {
			f, db, root := preparationFixture(t)
			defer db.Close()
			name := "file"
			if nested {
				name = "temporary/work/file"
				if err := os.MkdirAll(filepath.Join(f.root, "temporary/work"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			observeFile(t, db, f.root, name, []byte("temporary"))
			before, _, _ := db.Get(name)
			if err := os.RemoveAll(filepath.Join(f.root, strings.Split(name, "/")[0])); err != nil {
				t.Fatal(err)
			}
			w := &Worker{db: db, root: root, c: f.c, store: f.store, opts: WorkerOptions{Namespace: id("remote namespace")}}
			pass := workerPass{}
			deferred, err := w.deferLocalFailure(context.Background(), &localPathError{name, before.Generation, os.ErrNotExist}, &pass)
			if err != nil || !deferred || pass.retry == nil || pass.attention != nil {
				t.Fatal(deferred, err, pass)
			}
			after, _, _ := db.Get(name)
			if !after.Missing || after.Generation <= before.Generation {
				t.Fatal(after)
			}
			issue, err := db.SyncIssue(after)
			if err != nil || issue == nil || !issue.Retry {
				t.Fatal(issue, err)
			}
			if err := db.ClearTransientIssues(context.Background()); err != nil {
				t.Fatal(err)
			}
			result, err := Compare(context.Background(), db, root, f.c, &comparisonRemote{}, name, comparisonOptions())
			if err != nil || !result.Cleared || result.Local != nil {
				t.Fatal(result, err)
			}
		})
	}
}

func TestAncestorAbsenceRequiresOptInAndRejectsUnsafePaths(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	if _, missing, err := localMetadata(root, "missing/child/file", false); err == nil || missing {
		t.Fatal("strict metadata accepted missing parent", missing, err)
	}
	if _, missing, err := localMetadata(root, "missing/child/file", true); err != nil || !missing {
		t.Fatal("confirmed missing ancestor was not accepted", missing, err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(f.root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, missing, err := localMetadata(root, "link/child/file", true); err == nil || missing {
		t.Fatal("symlink accepted as absence", missing, err)
	}
	if err := os.Rename(f.root, f.root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, missing, err := localMetadata(root, "missing/child/file", true); err == nil || missing {
		t.Fatal("replaced root accepted as absence", missing, err)
	}
}
