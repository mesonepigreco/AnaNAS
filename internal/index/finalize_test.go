package index

import (
	"testing"

	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

func TestFinalizeUploadRollsBackWholeBatch(t *testing.T) {
	db := openSyncTest(t)
	u := uploadFixture(t, db)
	if err := db.Put([]Record{{Path: "second", Fingerprint: Fingerprint{Size: 1}}}, false); err != nil {
		t.Fatal(err)
	}
	u.Proposal.Entries = append(u.Proposal.Entries, journal.Entry{Path: "second", Next: journal.Version{ID: version("b").Content.ID, Size: 1, Digest: hash.SumBytes([]byte("b"))}})
	u.Generations = append(u.Generations, 1)
	u.DeltaBytes = append(u.DeltaBytes, 100)
	u.DeltaDigests = append(u.DeltaDigests, hash.SumBytes([]byte("second wire")))
	if err := db.PrepareUpload(u); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUploadCommit(u.Namespace, journal.Record{Proposal: u.Proposal, Sequence: 1, Epoch: 1, Committed: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.FinalizeUpload(u.Namespace, u.Proposal.ID, []*manifest.Manifest{version("a"), version("wrong")}); err == nil {
		t.Fatal("wrong second manifest accepted")
	}
	if base, err := db.Base("file"); err != nil || base != nil {
		t.Fatal("partial batch acknowledgement", base, err)
	}
	if pending, err := db.PendingUpload(); err != nil || pending == nil {
		t.Fatal(pending, err)
	}
	if err := db.Put([]Record{{Path: "second", Fingerprint: Fingerprint{Size: 2}}}, true); err != nil {
		t.Fatal(err)
	}
	if err := db.FinalizeUpload(u.Namespace, u.Proposal.ID, []*manifest.Manifest{version("a"), version("b")}); err != nil {
		t.Fatal(err)
	}
	if dirty, err := db.DirtyPage("", 2); err != nil || len(dirty) != 1 || dirty[0].Path != "second" || dirty[0].Generation != 2 {
		t.Fatal(dirty, err)
	}
}
