package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nas-sync/internal/journal"
)

func TestAlreadyMissingFileDeletionAndRecovery(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "still absent", true: "recreated before recovery"}[recreate], func(t *testing.T) {
			f := setup(t)
			data := []byte("original")
			base := candidate(t, f.state, 2, data)
			commit(t, f, 2, "file", "", base)
			if err := os.Remove(filepath.Join(f.root, "file")); err != nil {
				t.Fatal(err)
			}
			deletion := proposal(3, "file", base.ID, journal.Version{ID: id(3), Tombstone: true})
			crash := errors.New("interrupted after durable absence receipt")
			f.p.afterAbsenceReceipt = func() error { return crash }
			if _, err := f.c.Commit(context.Background(), f.c.Epoch(), deletion, f.p); !errors.Is(err, crash) {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(f.state, id(3)+".displaced")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unexpected displaced file", err)
			}
			// Reopen the coordinator and publisher like a daemon restart.
			f.p.Close()
			f.c.Close()
			var err error
			f.c, err = journal.Open(f.state, id(99))
			if err != nil {
				t.Fatal(err)
			}
			f.p, err = Open(f.root, f.state, f.c.Version, nil, 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			if recreate {
				// Even identical bytes are a new file after confirmed absence.
				if err := os.WriteFile(filepath.Join(f.root, "file"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); !errors.Is(err, ErrExternal) {
					t.Fatal(err)
				}
				contents(t, filepath.Join(f.root, "file"), data)
			} else {
				r, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p)
				if err != nil || r == nil || !r.Committed {
					t.Fatal(r, err)
				}
				if _, err := f.c.Commit(context.Background(), f.c.Epoch(), deletion, f.p); err != nil {
					t.Fatal("idempotent deletion", err)
				}
			}
			contents(t, filepath.Join(f.state, base.ID+".ready"), data)
		})
	}
}

func TestMissingDeletionDoesNotAcceptMissingParentOrHistory(t *testing.T) {
	for _, kind := range []string{"parent", "history", "replaced-root"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			if err := os.Mkdir(filepath.Join(f.root, "folder"), 0700); err != nil {
				t.Fatal(err)
			}
			base := candidate(t, f.state, 2, []byte("base"))
			commit(t, f, 2, "folder/file", "", base)
			if err := os.Remove(filepath.Join(f.root, "folder/file")); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "parent":
				if err := os.Remove(filepath.Join(f.root, "folder")); err != nil {
					t.Fatal(err)
				}
			case "history":
				if err := os.Remove(filepath.Join(f.state, base.ID+".ready")); err != nil {
					t.Fatal(err)
				}
			case "replaced-root":
				if err := os.Rename(f.root, f.root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(f.root, "folder"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(3, "folder/file", base.ID, journal.Version{ID: id(3), Tombstone: true}), f.p)
			if err == nil || !strings.Contains(err.Error(), "folder/file") {
				t.Fatal("missing failure/path context", err)
			}
			if _, err := os.Stat(filepath.Join(f.state, id(3)+".absent")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("false absence receipt", err)
			}
		})
	}
}
