package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"nas-sync/internal/journal"
)

func dirVersion(n int) journal.Version { return journal.Version{ID: id(n), Directory: true} }

func TestDirectoryTreeCreationAndBottomUpDeletion(t *testing.T) {
	f := setup(t)
	a, b := dirVersion(2), dirVersion(3)
	commit(t, f, 2, "a", "", a)
	commit(t, f, 3, "a/b", "", b)
	v := candidate(t, f.state, 4, []byte("nested file"))
	commit(t, f, 4, "a/b/file", "", v)
	contents(t, filepath.Join(f.root, "a/b/file"), []byte("nested file"))
	if st, err := os.Stat(filepath.Join(f.root, "a/b")); err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		t.Fatal(st, err)
	}
	commit(t, f, 5, "a/b/file", v.ID, journal.Version{ID: id(5), Tombstone: true})
	commit(t, f, 6, "a/b", b.ID, journal.Version{ID: id(6), Tombstone: true})
	commit(t, f, 7, "a", a.ID, journal.Version{ID: id(7), Tombstone: true})
	if _, err := os.Lstat(filepath.Join(f.root, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if changes, err := f.c.Changes(0, 8); err != nil || len(changes) != 6 {
		t.Fatal(changes, err)
	}
	contents(t, filepath.Join(f.state, id(5)+".displaced"), []byte("nested file"))
}

func TestDirectoryDeleteRefusesExistingOrRacingChildren(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing-excluded-child", true: "racing-child"}[race], func(t *testing.T) {
			f := setup(t)
			d := dirVersion(2)
			commit(t, f, 2, "folder", "", d)
			child := filepath.Join(f.root, "folder/keep")
			write := func() error { return os.WriteFile(child, []byte("keep me"), 0600) }
			if race {
				f.p.beforeRename = write
			} else {
				if err := write(); err != nil {
					t.Fatal(err)
				}
				f.p.Close()
				var err error
				f.p, err = Open(f.root, f.state, f.c.Version, []string{"keep"}, 1<<30)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(3, "folder", d.ID, journal.Version{ID: id(3), Tombstone: true}), f.p)
			if !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) {
				t.Fatal("nonempty directory delete not refused", err)
			}
			contents(t, child, []byte("keep me"))
			if _, pending, err := f.c.Head("folder"); err != nil || !pending {
				t.Fatal(pending, err)
			}
		})
	}
}

func TestDirectoryCreateAndDeleteRecovery(t *testing.T) {
	f := setup(t)
	d := dirVersion(2)
	f.p.afterRename = func() error { return context.Canceled }
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(2, "folder", "", d), f.p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.p.afterRename = nil
	if r, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); err != nil || r == nil || !r.Committed {
		t.Fatal(r, err)
	}
	if got, err := f.c.Version(d.ID); err != nil || !got.Directory {
		t.Fatal(got, err)
	}
	f.p.afterRename = func() error { return context.Canceled }
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(3, "folder", d.ID, journal.Version{ID: id(3), Tombstone: true}), f.p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.p.afterRename = nil
	if r, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); err != nil || r == nil || !r.Committed {
		t.Fatal(r, err)
	}
}

func TestDirectoryRecoveryDoesNotAdoptReplacementOrUndoDelete(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "replaced"}[replacement], func(t *testing.T) {
			f := setup(t)
			d := dirVersion(2)
			f.p.afterRename = func() error { return context.Canceled }
			if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(2, "folder", "", d), f.p); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			// Retain the original inode elsewhere so this test does not depend on
			// filesystem inode reuse. Inode recycling is a separate limitation.
			if err := os.Rename(filepath.Join(f.root, "folder"), filepath.Join(f.root, "moved")); err != nil {
				t.Fatal(err)
			}
			if replacement {
				if err := os.Mkdir(filepath.Join(f.root, "folder"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			f.p.afterRename = nil
			if _, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); !errors.Is(err, ErrExternal) {
				t.Fatal("recovery ignored directory replacement/deletion", err)
			}
		})
	}
}

func TestDirectoryExclusionAndTypeChanges(t *testing.T) {
	t.Run("directory-exclusion", func(t *testing.T) {
		f := setup(t)
		f.p.Close()
		var err error
		f.p, err = Open(f.root, f.state, f.c.Version, []string{"hidden/"}, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(2, "hidden", "", dirVersion(2)), f.p); err == nil {
			t.Fatal("excluded directory created")
		}
		if _, err := os.Lstat(filepath.Join(f.root, "hidden")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	})
	t.Run("file-to-directory", func(t *testing.T) {
		f := setup(t)
		v := candidate(t, f.state, 2, []byte("file"))
		commit(t, f, 2, "path", "", v)
		if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(3, "path", v.ID, dirVersion(3)), f.p); err == nil {
			t.Fatal("file replaced by directory")
		}
		contents(t, filepath.Join(f.root, "path"), []byte("file"))
	})
	t.Run("directory-to-file", func(t *testing.T) {
		f := setup(t)
		d := dirVersion(2)
		commit(t, f, 2, "path", "", d)
		v := candidate(t, f.state, 3, []byte("file"))
		if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(3, "path", d.ID, v), f.p); err == nil {
			t.Fatal("directory replaced by file")
		}
		if st, err := os.Stat(filepath.Join(f.root, "path")); err != nil || !st.IsDir() {
			t.Fatal(st, err)
		}
	})
}

func TestInterruptedDirectoryPreparation(t *testing.T) {
	f := setup(t)
	d := dirVersion(2)
	if err := os.Mkdir(filepath.Join(f.state, d.ID+".mkdir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, d.ID+".directory-writing"), []byte("partial"), 0400); err != nil {
		t.Fatal(err)
	}
	commit(t, f, 2, "folder", "", d)
	if _, err := os.Stat(filepath.Join(f.state, d.ID+".directory-writing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
