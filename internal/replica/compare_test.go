package replica

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/delta"
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

type comparisonRemote struct {
	v                 *journal.Version
	data              []byte
	heads, signatures int
	onHead            func()
}

func (r *comparisonRemote) CompareHead(context.Context, string, string) (*journal.Version, bool, error) {
	r.heads++
	if r.onHead != nil {
		r.onHead()
	}
	return r.v, false, nil
}

func (r *comparisonRemote) CompareSignature(ctx context.Context, _, _ string, _ journal.Version) (*delta.Signature, error) {
	r.signatures++
	return delta.Build(ctx, bytes.NewReader(r.data), int64(len(r.data)), 65536)
}

func comparisonOptions() ComparisonOptions {
	return ComparisonOptions{Namespace: id("remote namespace"), MaxFileBytes: 1 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }}
}

func observeFile(t *testing.T, db *index.DB, root, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: name, Fingerprint: content.Fingerprint(st)}}, true); err != nil {
		t.Fatal(err)
	}
}

func TestCompareAcknowledgedBaseClearsWithoutNetwork(t *testing.T) {
	f, db, u := completionFixture(t)
	defer db.Close()
	ctx := context.Background()
	if err := FinishUpload(ctx, db, f.c, f.store, u.Namespace, completionOptions()); err != nil {
		t.Fatal(err)
	}
	root, err := content.OpenRoot(f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	observeFile(t, db, f.root, "file", []byte("uploaded snapshot"))
	remote := &comparisonRemote{onHead: func() { t.Fatal("unchanged local content contacted remote") }}
	result, err := Compare(ctx, db, root, f.c, remote, "file", comparisonOptions())
	if err != nil || result.Decision.Action != diff.Noop || !result.Cleared {
		t.Fatal(result, err)
	}
	if dirty, err := db.DirtyPage("", 1); err != nil || len(dirty) != 0 {
		t.Fatal(dirty, err)
	}
	// Real changed content compares the immutable cached base after one head
	// request; rebuilding its signature on the NAS is unnecessary.
	observeFile(t, db, f.root, "file", []byte("new local content"))
	v := u.Proposal.Entries[0].Next
	remote = &comparisonRemote{v: &v}
	result, err = Compare(ctx, db, root, f.c, remote, "file", comparisonOptions())
	if err != nil || result.Decision.Action != diff.Push || result.Cleared || remote.heads != 1 || remote.signatures != 0 {
		t.Fatal(result, remote, err)
	}
	if dirty, err := db.DirtyPage("", 1); err != nil || len(dirty) != 1 {
		t.Fatal("decision erased untransferred work", dirty, err)
	}
}

func TestCompareConflictAndConcurrentEvent(t *testing.T) {
	f, db, u := completionFixture(t)
	defer db.Close()
	ctx := context.Background()
	if err := FinishUpload(ctx, db, f.c, f.store, u.Namespace, completionOptions()); err != nil {
		t.Fatal(err)
	}
	root, err := content.OpenRoot(f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	observeFile(t, db, f.root, "file", []byte("local change"))
	data := []byte("independent remote change")
	v := journal.Version{ID: id("remote change"), Size: int64(len(data)), Digest: hash.SumBytes(data)}
	remote := &comparisonRemote{v: &v, data: data}
	result, err := Compare(ctx, db, root, f.c, remote, "file", comparisonOptions())
	if err != nil || result.Decision.Action != diff.Conflict || result.Cleared || remote.signatures != 1 {
		t.Fatal(result, err)
	}
	state, err := db.PathState("file")
	if err != nil || state.Conflict != nil {
		t.Fatal("comparison claimed conflict-content retention", state, err)
	}
	remote.onHead = func() { observeFile(t, db, f.root, "file", []byte("edit during comparison")) }
	if _, err := Compare(ctx, db, root, f.c, remote, "file", comparisonOptions()); !errors.Is(err, index.ErrStale) {
		t.Fatal("new generation accepted", err)
	}
	if dirty, err := db.DirtyPage("", 1); err != nil || len(dirty) != 1 {
		t.Fatal(dirty, err)
	}
}

func TestCompareNewAndUnacknowledgedMissingPaths(t *testing.T) {
	f := setup(t)
	db, err := index.Open(filepath.Join(f.state, "index.db"), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root, err := content.OpenRoot(f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	observeFile(t, db, f.root, "file", []byte("new file"))
	remote := &comparisonRemote{}
	result, err := Compare(context.Background(), db, root, f.c, remote, "file", comparisonOptions())
	if err != nil || result.Decision.Action != diff.Push || result.Cleared {
		t.Fatal(result, err)
	}
	if err := os.Remove(filepath.Join(f.root, "file")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]index.Record{{Path: "file", Missing: true}}, true); err != nil {
		t.Fatal(err)
	}
	data := []byte("remote existing file")
	v := journal.Version{ID: id("existing remote"), Size: int64(len(data)), Digest: hash.SumBytes(data)}
	remote.v, remote.data = &v, data
	result, err = Compare(context.Background(), db, root, f.c, remote, "file", comparisonOptions())
	if err != nil || result.Decision.Action != diff.Pull || result.Local != nil || result.Cleared {
		t.Fatal("unacknowledged absence became delete", result, err)
	}
	remote.v = nil
	result, err = Compare(context.Background(), db, root, f.c, remote, "file", comparisonOptions())
	if err != nil || result.Decision.Action != diff.Noop || !result.Cleared || result.Local != nil {
		t.Fatal("absent temporary file left perpetual dirty work", result, err)
	}
	if base, err := db.Base("file"); err != nil || base != nil {
		t.Fatal("unacknowledged absence created a tombstone", base, err)
	}
}
