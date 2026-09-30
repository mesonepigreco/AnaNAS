package index

import (
	"context"
	"path/filepath"
	"testing"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

func treeVersion(path string) *manifest.Manifest {
	m := version("a")
	m.Content.ID = hash.SumBytes([]byte(path)).Hex()
	return m
}
func treePage(t *testing.T, paths ...string) changefeed.Page {
	t.Helper()
	p := remotePage(t, false)
	p.Batches = nil
	for i, path := range paths {
		v := treeVersion(path)
		p.Batches = append(p.Batches, changefeed.Batch{Proposal: journal.Proposal{ID: hash.SumBytes([]byte("op" + path)).Hex(), Client: hash.SumBytes([]byte("client")).Hex(), Entries: []journal.Entry{{Path: path, Next: journal.Version{ID: v.Content.ID, Size: v.Content.Size, Digest: hash.SumBytes([]byte(path))}}}}, Sequence: uint64(i + 1), Epoch: 1, Committed: true})
	}
	p.Through = uint64(len(paths))
	return p
}
func finishTreeBatch(t *testing.T, d *DB, p changefeed.Page, b *changefeed.Batch) {
	t.Helper()
	manifests := make([]*manifest.Manifest, len(b.Entries))
	for i, e := range b.Entries {
		manifests[i] = treeVersion(e.Path)
	}
	if err := d.FinalizeDownload(p.Namespace, b.Sequence, b.ID, manifests); err != nil {
		t.Fatal(err)
	}
}
func TestDirectoryPriorityPinsCurrentTransferAndKeepsAckPrefixAcrossRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "index.db")
	d, err := Open(file, "/local")
	if err != nil {
		t.Fatal(err)
	}
	p := treePage(t, "a/current", "a/later", "z/urgent")
	if err := d.PrefetchRemotePage(p); err != nil {
		t.Fatal(err)
	}
	current, err := d.NextRemoteBatch()
	if err != nil || current.Sequence != 1 {
		t.Fatal(current, err)
	}
	if err := d.PrioritizeDirectory("z"); err != nil {
		t.Fatal(err)
	}
	still, err := d.NextRemoteBatch()
	if err != nil || still.ID != current.ID {
		t.Fatal("priority interrupted current transfer", still, err)
	}
	d.Close()
	d, err = Open(file, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	still, err = d.NextRemoteBatch()
	if err != nil || still.ID != current.ID {
		t.Fatal("restart lost pinned transfer", still, err)
	}
	finishTreeBatch(t, d, p, current)
	urgent, err := d.NextRemoteBatch()
	if err != nil || urgent.Sequence != 3 {
		t.Fatal("directory did not jump queue", urgent, err)
	}
	finishTreeBatch(t, d, p, urgent)
	state, _ := d.RemoteState()
	if state.Completed != 1 {
		t.Fatal("acknowledged unsynced gap", state)
	}
	if err := d.RecordRemoteAcknowledged(p.Namespace, p.Policy, 0, 3); err == nil {
		t.Fatal("acknowledged gap")
	}
	d.Close()
	d, err = Open(file, "/local")
	if err != nil {
		t.Fatal(err)
	}
	next, err := d.NextRemoteBatch()
	if err != nil || next.Sequence != 2 {
		t.Fatal(next, err)
	}
	finishTreeBatch(t, d, p, next)
	state, _ = d.RemoteState()
	if state.Completed != 3 {
		t.Fatal("completion did not advance over durable gap", state)
	}
	if err := d.RecordRemoteAcknowledged(p.Namespace, p.Policy, 0, 3); err != nil {
		t.Fatal(err)
	}
	d.Close()
}
func TestDirectoryPriorityRespectsEarlierParentAndPathOperations(t *testing.T) {
	d := openSyncTest(t)
	p := treePage(t, "parent", "other/file", "parent/child/file")
	if err := d.PrefetchRemotePage(p); err != nil {
		t.Fatal(err)
	}
	if err := d.PrioritizeDirectory("parent/child"); err != nil {
		t.Fatal(err)
	}
	first, err := d.NextRemoteBatch()
	if err != nil || first.Sequence != 1 {
		t.Fatal("parent dependency skipped", first, err)
	}
	for _, p := range []string{"../escape", "/absolute", "a/../b", ".nas-sync/hidden"} {
		if err := d.PrioritizeDirectory(p); err == nil {
			t.Fatal("invalid priority accepted", p)
		}
	}
}
func TestNASInventoryIncludesRemoteOnlyAndChecksExactVersions(t *testing.T) {
	d := openSyncTest(t)
	p := treePage(t, "folder/local", "folder/remote")
	if err := d.RecordNASPage(p); err != nil {
		t.Fatal(err)
	}
	tree, err := d.NASTree(context.Background(), "", "")
	if err != nil || tree.Files != 2 || tree.SyncedFiles != 0 || len(tree.Children) != 1 {
		t.Fatal(tree, err)
	}
	if err := d.Put([]Record{{Path: "folder/local", Fingerprint: Fingerprint{Size: treeVersion("folder/local").Content.Size}}}, false); err != nil {
		t.Fatal(err)
	}
	r, _, _ := d.Get("folder/local")
	if _, err := d.Acknowledge("folder/local", "", r.Generation, treeVersion("folder/local")); err != nil {
		t.Fatal(err)
	}
	tree, err = d.NASTree(context.Background(), "folder", "")
	if err != nil || tree.SyncedFiles != 1 || len(tree.Children) != 2 {
		t.Fatal(tree, err)
	}
	// Same-size local edits must reduce completion rather than remaining green.
	if err := d.Put([]Record{r}, true); err != nil {
		t.Fatal(err)
	}
	tree, err = d.NASTree(context.Background(), "folder", "")
	if err != nil || tree.SyncedFiles != 0 {
		t.Fatal(tree, err)
	}
	state, _ := d.RemoteState()
	if state.Received != 0 || state.Completed != 0 {
		t.Fatal("inventory acknowledged downloads", state)
	}
}
