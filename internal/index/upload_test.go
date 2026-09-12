package index

import (
	"errors"
	"path/filepath"
	"testing"

	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
)

func uploadFixture(t *testing.T, d *DB) Upload {
	t.Helper()
	client, err := d.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put([]Record{{Path: "file", Fingerprint: Fingerprint{Size: 1}}}, false); err != nil {
		t.Fatal(err)
	}
	return Upload{Namespace: hash.SumBytes([]byte("namespace")).Hex(), Proposal: journal.Proposal{ID: hash.SumBytes([]byte("upload")).Hex(), Client: client, Entries: []journal.Entry{{Path: "file", Next: journal.Version{ID: version("a").Content.ID, Size: 1, Digest: hash.SumBytes([]byte("a"))}}}}, Generations: []uint64{1}, DeltaBytes: []int64{100}, DeltaDigests: []hash.Digest{hash.SumBytes([]byte("wire"))}}
}

func TestUploadOutboxRestartAndNewerEdit(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "index.db")
	d, err := Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	u := uploadFixture(t, d)
	if err := d.PrepareUpload(u); err != nil {
		t.Fatal(err)
	}
	if err := d.PrepareUpload(u); err != nil {
		t.Fatal("idempotent prepare", err)
	}
	if err := d.CompleteUpload(u.Proposal.ID); !errors.Is(err, ErrStale) {
		t.Fatal("completed before commit", err)
	}
	d.Close()
	d, err = Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pending, err := d.PendingUpload()
	if err != nil || pending == nil || pending.Proposal.ID != u.Proposal.ID || pending.Receipt != nil {
		t.Fatal(pending, err)
	}
	other := u
	other.Proposal.ID = hash.SumBytes([]byte("another upload")).Hex()
	if err := d.PrepareUpload(other); !errors.Is(err, ErrStale) {
		t.Fatal("unbounded outbox", err)
	}
	committed := journal.Record{Proposal: u.Proposal, Sequence: 1, Epoch: 2, Committed: true}
	if err := d.RecordUploadCommit(u.Namespace, committed); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordUploadCommit(u.Namespace, committed); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteUpload(u.Proposal.ID); !errors.Is(err, ErrStale) {
		t.Fatal("commit receipt cleared unapplied bookkeeping", err)
	}
	if err := d.Put([]Record{{Path: "file", Fingerprint: Fingerprint{Size: 2}}}, true); err != nil {
		t.Fatal(err)
	}
	if cleared, err := d.Acknowledge("file", "", 1, version("a")); err != nil || cleared {
		t.Fatal("newer edit lost", cleared, err)
	}
	if err := d.CompleteUpload(u.Proposal.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteUpload(u.Proposal.ID); err != nil {
		t.Fatal("completion retry", err)
	}
	if pending, err := d.PendingUpload(); err != nil || pending != nil {
		t.Fatal(pending, err)
	}
	if dirty, err := d.DirtyPage("", 1); err != nil || len(dirty) != 1 || dirty[0].Generation != 2 {
		t.Fatal(dirty, err)
	}
}

func TestUploadRejectsWrongReceiptAndPreparation(t *testing.T) {
	for _, mode := range []string{"generation", "client", "paused", "excluded", "wire", "namespace", "type", "size", "missing"} {
		t.Run(mode, func(t *testing.T) {
			d := openSyncTest(t)
			u := uploadFixture(t, d)
			switch mode {
			case "type":
				u.Proposal.Entries[0].Next = journal.Version{ID: version("a").Content.ID, Directory: true}
				u.DeltaBytes[0] = 0
				u.DeltaDigests[0] = hash.Digest{}
			case "size":
				u.Proposal.Entries[0].Next.Size = 2
			case "missing":
				u.Proposal.Entries[0].Next = journal.Version{ID: version("a").Content.ID, Tombstone: true}
				u.DeltaBytes[0] = 0
				u.DeltaDigests[0] = hash.Digest{}
			case "generation":
				u.Generations[0]++
			case "client":
				u.Proposal.Client = hash.SumBytes([]byte("other")).Hex()
			case "paused":
				if err := d.SetPaused(true); err != nil {
					t.Fatal(err)
				}
			case "excluded":
				if err := d.Put([]Record{{Path: "file", Excluded: true}}, false); err != nil {
					t.Fatal(err)
				}
			case "wire":
				u.DeltaBytes[0] = 1
			case "namespace":
				page := remotePage(t, true)
				if err := d.ReceiveRemotePage(page); err != nil {
					t.Fatal(err)
				}
				u.Namespace = hash.SumBytes([]byte("wrong namespace")).Hex()
			}
			if err := d.PrepareUpload(u); err == nil {
				t.Fatal("unsafe preparation accepted")
			}
			if pending, err := d.PendingUpload(); err != nil || pending != nil {
				t.Fatal("failed prepare persisted", pending, err)
			}
		})
	}
	d := openSyncTest(t)
	u := uploadFixture(t, d)
	if err := d.PrepareUpload(u); err != nil {
		t.Fatal(err)
	}
	record := journal.Record{Proposal: u.Proposal, Sequence: 1, Epoch: 1}
	if err := d.RecordUploadCommit(u.Namespace, record); err == nil {
		t.Fatal("prepared remote work marked committed")
	}
	record.Committed = true
	if err := d.RecordUploadCommit(hash.SumBytes([]byte("wrong namespace")).Hex(), record); err == nil {
		t.Fatal("wrong namespace acknowledged")
	}
	record.Proposal.ID = hash.SumBytes([]byte("another operation")).Hex()
	if err := d.RecordUploadCommit(u.Namespace, record); err == nil {
		t.Fatal("wrong operation acknowledged")
	}
}
