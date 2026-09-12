package content

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMetadataRequiresExactMissingLeafAndRejectsUnsafeTypes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "parent"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir, []string{"excluded/"})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, missing, err := root.Metadata("parent/missing"); err != nil || !missing {
		t.Fatal("exact leaf absence rejected", missing, err)
	}
	if _, missing, err := root.Metadata("missing-parent/child"); err == nil || missing {
		t.Fatal("missing parent inferred deletion", missing, err)
	}
	for _, name := range []string{"link", "fifo", "../outside", ".nas-sync/file", "excluded/file"} {
		if _, missing, err := root.Metadata(name); err == nil || missing {
			t.Fatal("unsafe path accepted", name, missing, err)
		}
	}
	if fp, missing, err := root.Metadata("file"); err != nil || missing || fp.Size != 4 || !os.FileMode(fp.Mode).IsRegular() {
		t.Fatal(fp, missing, err)
	}
	if fp, missing, err := root.Metadata("parent"); err != nil || missing || !os.FileMode(fp.Mode).IsDir() {
		t.Fatal(fp, missing, err)
	}
}
