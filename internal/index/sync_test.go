package index

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"

	bolt "go.etcd.io/bbolt"
)

func version(id string) *manifest.Manifest {
	return &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: strings.Repeat(id, 64), Size: 1,
		Blocks: []diff.Block{{Digest: hash.SumBytes([]byte(id)), Size: 1}}}}
}

func openSyncTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "index.db"), "/local")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestAckPreservesInterveningEditAndRejectsStaleCompletion(t *testing.T) {
	d := openSyncTest(t)
	r := Record{Path: "file", Fingerprint: Fingerprint{Size: 1}}
	if err := d.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	if err := d.Put([]Record{r}, true); err != nil {
		t.Fatal(err)
	}
	cleared, err := d.Acknowledge("file", "", 1, version("a"))
	if err != nil || cleared {
		t.Fatalf("ack erased newer edit: %v %v", cleared, err)
	}
	page, err := d.DirtyPage("", 1)
	if err != nil || len(page) != 1 || page[0].Generation != 2 {
		t.Fatalf("pending work lost: %v %v", page, err)
	}
	if _, err := d.Acknowledge("file", "", 2, version("b")); !errors.Is(err, ErrStale) {
		t.Fatalf("stale base accepted: %v", err)
	}
	base, err := d.Base("file")
	if err != nil || base.Content.ID != version("a").Content.ID {
		t.Fatal("base changed on rejected ack", err)
	}
	cleared, err = d.Acknowledge("file", base.Content.ID, 2, version("b"))
	if err != nil || !cleared {
		t.Fatalf("current ack failed: %v %v", cleared, err)
	}
	page, err = d.DirtyPage("", 1)
	if err != nil || len(page) != 0 {
		t.Fatalf("completed work still dirty: %v %v", page, err)
	}
	r.Scan = 10
	if err := d.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	page, err = d.DirtyPage("", 1)
	if err != nil || len(page) != 0 {
		t.Fatal("seen-only scan requeued completed work")
	}
}

func TestDirtyPaginationExclusionsAndMigration(t *testing.T) {
	d := openSyncTest(t)
	if err := d.Put([]Record{{Path: "a"}, {Path: "b", Excluded: true}, {Path: "c"}, {Path: "d", Missing: true}}, false); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		cursor := ""
		for _, want := range []string{"a", "c", "d"} {
			p, err := d.DirtyPage(cursor, 1)
			if err != nil || len(p) != 1 || p[0].Path != want {
				t.Fatalf("got %v %v; want %s", p, err, want)
			}
			cursor = p[0].Path
		}
		p, err := d.DirtyPage(cursor, 1)
		if err != nil || len(p) != 0 {
			t.Fatalf("unexpected trailing page: %v %v", p, err)
		}
	}
	check()
	// Recreate only the migration condition of a pre-sync-state database.
	if err := d.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(dirty) }); err != nil {
		t.Fatal(err)
	}
	if err := d.db.Update(initSyncState); err != nil {
		t.Fatal(err)
	}
	check()
	for _, n := range []int{0, 1001} {
		if _, err := d.DirtyPage("", n); err == nil {
			t.Fatal("accepted unbounded page")
		}
	}
}

func TestSyncStateSurvivesRestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "i.db")
	d, err := Open(p, "/local")
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.ClientID()
	if err != nil || !manifest.ValidID(id) {
		t.Fatalf("client identity: %q %v", id, err)
	}
	if err := d.Put([]Record{{Path: "file", Fingerprint: Fingerprint{Size: 1}}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Acknowledge("file", "", 1, version("a")); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	c := Conflict{ID: strings.Repeat("d", 64), BaseID: version("a").Content.ID, LocalID: version("b").Content.ID, RemoteID: version("c").Content.ID, Generation: 1}
	if err := d.RecordConflict("file", c); err != nil {
		t.Fatal(err)
	}
	if err := d.KeepDifferent("file", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(p, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id2, err := d.ClientID()
	if err != nil || id2 != id {
		t.Fatal("client ID changed across restart", err)
	}
	paused, err := d.Paused()
	if err != nil || !paused {
		t.Fatal("global pause lost", err)
	}
	s, err := d.PathState("file")
	if err != nil || !s.Paused || s.Conflict == nil || *s.Conflict != c {
		t.Fatalf("conflict pause lost: %+v %v", s, err)
	}
	base, err := d.Base("file")
	if err != nil || base.Content.ID != c.BaseID {
		t.Fatal("acknowledged base lost", err)
	}
	if err := d.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	s, err = d.PathState("file")
	if err != nil || !s.Paused {
		t.Fatal("global resume erased per-path pause")
	}
}

func TestRejectStaleConflictChoicesAndUnsafePaths(t *testing.T) {
	d := openSyncTest(t)
	r := Record{Path: "file"}
	if err := d.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	c := Conflict{ID: strings.Repeat("a", 64), LocalID: strings.Repeat("b", 64), RemoteID: strings.Repeat("c", 64), Generation: 1}
	if err := d.RecordConflict("file", c); err != nil {
		t.Fatal(err)
	}
	if err := d.KeepDifferent("file", strings.Repeat("d", 64)); !errors.Is(err, ErrStale) {
		t.Fatal("stale UI choice accepted", err)
	}
	c.ID = strings.Repeat("d", 64)
	if err := d.RecordConflict("file", c); !errors.Is(err, ErrStale) {
		t.Fatal("unresolved conflict overwritten", err)
	}
	if err := d.Put([]Record{r}, true); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordConflict("file", c); !errors.Is(err, ErrStale) {
		t.Fatal("stale generation accepted", err)
	}
	for _, p := range []string{"", ".", "..", "a/../b", "/absolute", "a//b", "a\\b", "a/.nas-sync/private", "a\x00b"} {
		if _, err := d.Base(p); err == nil {
			t.Fatalf("unsafe path accepted: %q", p)
		}
	}
}

func TestExcludedAndFutureGenerationCannotAcknowledge(t *testing.T) {
	d := openSyncTest(t)
	if err := d.Put([]Record{{Path: "excluded", Excluded: true}, {Path: "live"}}, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"excluded", "unknown", "live"} {
		if _, err := d.Acknowledge(p, "", 2, version("a")); !errors.Is(err, ErrStale) {
			t.Fatalf("unsafe ack %s: %v", p, err)
		}
		m, err := d.Base(p)
		if err != nil || m != nil {
			t.Fatal("failed ack left a base", err)
		}
	}
}
