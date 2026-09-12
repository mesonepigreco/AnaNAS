package index

import (
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"
	"os"
	"path/filepath"
	"testing"
)

func TestRetireArchiveOnlyDirectoriesAndAtomic(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "index.db"), "/local")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.Put([]Record{{Path: "folder", Fingerprint: Fingerprint{Mode: uint32(os.ModeDir)}}, {Path: "file", Fingerprint: Fingerprint{Size: 5}}}, false); e != nil {
		t.Fatal(e)
	}
	if e = db.RetireArchivedDirectories([]string{"folder", "file"}); e == nil {
		t.Fatal("retired a file")
	}
	state, _ := db.PathState("folder")
	if state.Paused {
		t.Fatal("partial retirement")
	}
	if e = db.RetireArchivedDirectories([]string{"folder"}); e != nil {
		t.Fatal(e)
	}
	state, _ = db.PathState("folder")
	if !state.Paused {
		t.Fatal("old directory not retired")
	}
	state, _ = db.PathState("file")
	if state.Paused {
		t.Fatal("file deletion would be blocked")
	}
}

func TestArchiveTombstoneRetirementRequiresAcknowledgedAbsence(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "index.db"), "/local")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.Put([]Record{{Path: "old/file", Missing: true}}, false); e != nil {
		t.Fatal(e)
	}
	if e = db.RetireArchivedTombstones([]string{"old/file"}); e == nil {
		t.Fatal("retired without acknowledged deletion")
	}
	m := &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: hash.SumBytes([]byte("deleted")).Hex(), Tombstone: true}}
	if _, e = db.Acknowledge("old/file", "", 1, m); e != nil {
		t.Fatal(e)
	}
	if e = db.RetireArchivedTombstones([]string{"old/file"}); e != nil {
		t.Fatal(e)
	}
	state, e := db.PathState("old/file")
	if e != nil || !state.Paused {
		t.Fatal(state, e)
	}
}
