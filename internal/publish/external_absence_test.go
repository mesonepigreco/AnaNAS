package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

func TestExternalAbsenceReceiptSurvivesRestartAndRecreation(t *testing.T) {
	f := setup(t)
	if err := f.c.ConfigurePublicationBudget(journal.PublicationBudget{MaxBytes: 4 << 20, MaxEntries: 10}); err != nil {
		t.Fatal(err)
	}
	data := []byte("retained original")
	base := candidate(t, f.state, 2, data)
	commit(t, f, 2, "file", "", base)
	info, err := os.Stat(filepath.Join(f.root, "file"))
	if err != nil {
		t.Fatal(err)
	}
	want := content.Fingerprint(info)
	s, err := stage.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.Remove(filepath.Join(f.root, "file")); err != nil {
		t.Fatal(err)
	}
	p := proposal(3, "file", base.ID, journal.Version{ID: id(3), Tombstone: true})
	interrupted := errors.New("sealed absence before commit")
	_, err = f.c.ImportExternal(context.Background(), f.c.Epoch(), p, func(ctx context.Context, resume bool) (journal.Version, error) {
		v, err := f.p.CaptureExternal(ctx, p.Entries[0], &want, s, resume)
		if err != nil {
			return v, err
		}
		return v, interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "file"), []byte("later native creation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c, err = journal.Open(f.state, id(99))
	if err != nil {
		t.Fatal(err)
	}
	f.p.lookup = f.c.Version
	r, err := f.c.ImportExternal(context.Background(), f.c.Epoch(), p, func(ctx context.Context, resume bool) (journal.Version, error) {
		return f.p.CaptureExternal(ctx, p.Entries[0], nil, s, resume)
	})
	if err != nil || !r.Committed || !r.Entries[0].Next.Tombstone {
		t.Fatal(r, err)
	}
	contents(t, filepath.Join(f.root, "file"), []byte("later native creation"))
	contents(t, filepath.Join(f.state, base.ID+".ready"), data)
}

func TestExternalAbsenceRefusesUncertainPathAndRestoresCharge(t *testing.T) {
	for _, kind := range []string{"present", "missing-parent", "symlink-parent", "inaccessible-parent", "replaced-root", "sealed-abort"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			if err := f.c.ConfigurePublicationBudget(journal.PublicationBudget{MaxBytes: 4 << 20, MaxEntries: 10}); err != nil {
				t.Fatal(err)
			}
			commit(t, f, 2, "folder", "", dirVersion(2))
			base := candidate(t, f.state, 3, []byte("original"))
			commit(t, f, 3, "folder/file", "", base)
			info, err := os.Stat(filepath.Join(f.root, "folder/file"))
			if err != nil {
				t.Fatal(err)
			}
			want := content.Fingerprint(info)
			s, err := stage.Open(f.state)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if kind != "present" {
				if err := os.Remove(filepath.Join(f.root, "folder/file")); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "missing-parent", "symlink-parent":
				if err := os.Rename(filepath.Join(f.root, "folder"), filepath.Join(f.root, "moved")); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink-parent" {
					if err := os.Symlink("moved", filepath.Join(f.root, "folder")); err != nil {
						t.Fatal(err)
					}
				}
			case "inaccessible-parent":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory permissions")
				}
				if err := os.Chmod(filepath.Join(f.root, "folder"), 0); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(filepath.Join(f.root, "folder"), 0700)
			case "replaced-root":
				if err := os.Rename(f.root, f.root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(f.root, "folder"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			usage, err := f.c.PublicationBudgetUsage()
			if err != nil {
				t.Fatal(err)
			}
			p := proposal(4, "folder/file", base.ID, journal.Version{ID: id(4), Tombstone: true})
			_, err = f.c.ImportExternal(context.Background(), f.c.Epoch(), p, func(ctx context.Context, resume bool) (journal.Version, error) {
				v, err := f.p.CaptureExternal(ctx, p.Entries[0], &want, s, resume)
				if kind == "sealed-abort" && err == nil {
					return v, errors.New("abort sealed receipt")
				}
				return v, err
			})
			if err == nil {
				t.Fatal("uncertain or interrupted deletion committed")
			}
			if err := f.c.AbortExternal(context.Background(), f.c.Epoch(), p.ID); err != nil {
				t.Fatal(err)
			}
			after, err := f.c.PublicationBudgetUsage()
			if err != nil || *after != *usage {
				t.Fatal(usage, after, err)
			}
			if _, err := os.Lstat(filepath.Join(f.state, id(4)+".absent")); !os.IsNotExist(err) {
				t.Fatal("receipt retained after abort", err)
			}
			head, pending, err := f.c.Head("folder/file")
			if err != nil || pending || head == nil || head.ID != base.ID {
				t.Fatal("head changed after refusal", head, pending, err)
			}
			contents(t, filepath.Join(f.state, base.ID+".ready"), []byte("original"))
		})
	}
}
