package index

import (
	"testing"

	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

func TestFinalizeDownloadAtomicAndPreservesObserverState(t *testing.T) {
	db := openSyncTest(t)
	p := remotePage(t, false)
	p.Batches[0].Entries = append(p.Batches[0].Entries, journal.Entry{Path: "second", Next: journal.Version{ID: version("b").Content.ID, Size: 1, Digest: hash.SumBytes([]byte("b"))}})
	if err := db.ReceiveRemotePage(p); err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]Record{{Path: "second", Fingerprint: Fingerprint{Size: 200}}}, true); err != nil {
		t.Fatal(err)
	}
	before, ok, err := db.Get("second")
	if err != nil || !ok {
		t.Fatal(before, err)
	}
	if err := db.FinalizeDownload(p.Namespace, 1, p.Batches[0].ID, []*manifest.Manifest{version("a"), version("wrong")}); err == nil {
		t.Fatal("wrong manifest accepted")
	}
	if base, err := db.Base("file"); err != nil || base != nil {
		t.Fatal("partial base committed", base, err)
	}
	if s, err := db.RemoteState(); err != nil || s.Completed != 0 {
		t.Fatal(s, err)
	}
	if err := db.FinalizeDownload(p.Namespace, 1, p.Batches[0].ID, []*manifest.Manifest{version("a"), version("b")}); err != nil {
		t.Fatal(err)
	}
	after, ok, err := db.Get("second")
	if err != nil || !ok || before != after || !after.Dirty {
		t.Fatal(before, after, err)
	}
	if _, ok, err := db.Get("file"); err != nil || ok {
		t.Fatal("completion invented an observed file", ok, err)
	}
	if base, err := db.Base("file"); err != nil || base == nil || base.Content.ID != version("a").Content.ID {
		t.Fatal(base, err)
	}
	if s, err := db.RemoteState(); err != nil || s.Completed != 1 {
		t.Fatal(s, err)
	}
}
