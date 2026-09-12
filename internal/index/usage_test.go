package index

import (
	"context"
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoryUsageCountsLogicalFilesAndRespectsPathBoundary(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "index.db"), "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	records := []Record{
		{Path: "a", Fingerprint: Fingerprint{Mode: uint32(os.ModeDir), Size: 4096}},
		{Path: "a/file", Fingerprint: Fingerprint{Size: 12}},
		{Path: "a/sub/file", Fingerprint: Fingerprint{Size: 34}},
		{Path: "ab/other", Fingerprint: Fingerprint{Size: 999}},
		{Path: "a/missing", Missing: true, Fingerprint: Fingerprint{Size: 999}},
		{Path: "a/excluded", Excluded: true, Fingerprint: Fingerprint{Size: 999}},
		{Path: "a/link", Fingerprint: Fingerprint{Mode: uint32(os.ModeSymlink), Size: 999}},
	}
	if err = db.Put(records, false); err != nil {
		t.Fatal(err)
	}
	u, err := db.DirectoryUsage(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if u.LocalBytes != 46 || u.Files != 2 || len(u.Children) != 1 || u.Children[0].Path != "a/sub" || u.Children[0].LocalBytes != 34 {
		t.Fatalf("%+v", u)
	}
	if _, err = db.DirectoryUsage(context.Background(), "../a"); err == nil {
		t.Fatal("accepted traversal")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = db.DirectoryUsage(ctx, ""); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestRetiredMissingDirectoryDoesNotAppearAsGhostNASFolder(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "index.db"), "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := Record{Path: "retired", Fingerprint: Fingerprint{Mode: uint32(os.ModeDir)}}
	if err = db.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	base := &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: hash.SumBytes([]byte("directory")).Hex(), Directory: true}}
	if _, err = db.Acknowledge("retired", "", 1, base); err != nil {
		t.Fatal(err)
	}
	if err = db.RetireArchivedDirectories([]string{"retired"}); err != nil {
		t.Fatal(err)
	}
	r.Missing = true
	if err = db.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	usage, err := db.DirectoryUsage(context.Background(), "")
	if err != nil || len(usage.Children) != 0 {
		t.Fatal(usage, err)
	}
	if old, err := db.Base("retired"); err != nil || old == nil {
		t.Fatal("history removed", err)
	}
}
