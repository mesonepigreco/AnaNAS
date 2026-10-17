package publish

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryBackupIsIdempotentAndNeverFollowsSymlinks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(f.root, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "folder/file"), []byte("unsent work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.p.PreserveForResolution(ctx, "folder", id(8)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.root, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "folder/new"), []byte("newer edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.p.PreserveForResolution(ctx, "folder", id(8)); err != nil {
		t.Fatal(err)
	}
	contents(t, filepath.Join(f.state, "recovered", "folder-"+id(8), "file"), []byte("unsent work"))
	contents(t, filepath.Join(f.root, "folder/new"), []byte("newer edit"))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := f.p.PreserveForResolution(ctx, "link", id(9)); err == nil {
		t.Fatal("symlink moved or followed")
	}
	if err := f.p.PreserveForResolution(ctx, "link/child", id(10)); err == nil {
		t.Fatal("symlink ancestor followed")
	}
}
