package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPendingHidesInternalTrialsButRetainsRecoveryRecords(t *testing.T) {
	db := openSyncTest(t)
	ctx := context.Background()
	records := []Record{{Path: "anaNAS-deletions-bbbbbbbbbbbb/probe.bin"}, {Path: "anaNAS-folders-aaaaaaaaaaaa"}, {Path: ".ananas-tests/probe"}, {Path: "anaNAS-folders-my-work"}, {Path: "normal/file"}}
	if err := db.Put(records, false); err != nil {
		t.Fatal(err)
	}
	p, err := db.PendingSync(ctx, "", 10)
	if err != nil || p.Total != 2 || len(p.Items) != 2 {
		t.Fatal(p, err)
	}
	confirmed, err := db.ConfirmSync(ctx, SyncConfirmation{All: true}, 10)
	if err != nil || confirmed.Queued != 2 {
		t.Fatal(confirmed, err)
	}
	for _, want := range records[:3] {
		r, ok, err := db.Get(want.Path)
		if err != nil || !ok || !r.Dirty {
			t.Fatal("lost trial recovery record", r, err)
		}
	}
}

func TestPendingConfirmationPersistsWithoutAcknowledgingAndIncludesParents(t *testing.T) {
	name := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(name, "/local")
	if err != nil {
		t.Fatal(err)
	}
	r := Record{Path: "folder/file", Fingerprint: Fingerprint{Size: 1}}
	if err := db.Put([]Record{{Path: "folder", Fingerprint: Fingerprint{Mode: uint32(os.ModeDir)}}, r}, false); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	result, err := db.ConfirmSync(ctx, SyncConfirmation{Path: r.Path, Generation: 1}, 10)
	if err != nil || result.Queued != 2 {
		t.Fatal(result, err)
	}
	db.Close()
	db, err = Open(name, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	page, err := db.RequestedPage("", 10)
	if err != nil || len(page) != 2 || page[0].Path != "folder" || !page[1].Dirty {
		t.Fatal(page, err)
	}
	base, err := db.Base(r.Path)
	if err != nil || base != nil {
		t.Fatal("confirmation invented a NAS acknowledgement", base, err)
	}
	if _, err := db.Acknowledge(r.Path, "", 1, version("a")); err != nil {
		t.Fatal(err)
	}
	page, err = db.RequestedPage("", 10)
	if err != nil || len(page) != 1 || page[0].Path != "folder" {
		t.Fatal(page, err)
	}
	if err := db.Put([]Record{r}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConfirmSync(ctx, SyncConfirmation{Path: r.Path, Generation: 1}, 10); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
}

func TestConfirmAllIncludesUnshownPagesAndSkipsBlockedFiles(t *testing.T) {
	db := openSyncTest(t)
	var records []Record
	for i := 0; i < 205; i++ {
		records = append(records, Record{Path: fmt.Sprintf("file-%03d", i), Fingerprint: Fingerprint{Size: 1}})
	}
	records = append(records, Record{Path: "too-large", Fingerprint: Fingerprint{Size: 11}}, Record{Path: "excluded", Excluded: true})
	if err := db.Put(records, false); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page, err := db.PendingSync(ctx, "", 10)
	if err != nil || page.Total != 206 || len(page.Items) != 200 || page.Next != "file-199" {
		t.Fatal(page, err)
	}
	last, err := db.PendingSync(ctx, page.Next, 10)
	if err != nil || len(last.Items) != 6 || last.Next != "" || last.Items[5].Confirmable {
		t.Fatal(last, err)
	}
	if err := db.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	result, err := db.ConfirmSync(ctx, SyncConfirmation{All: true}, 10)
	if err != nil || result.Queued != 205 || result.Blocked != 1 {
		t.Fatal(result, err)
	}
	paused, err := db.Paused()
	if err != nil || !paused {
		t.Fatal("confirmation resumed paused sync", err)
	}
	queued, err := db.RequestedPage("", 1000)
	if err != nil || len(queued) != 205 {
		t.Fatal(len(queued), err)
	}
	for _, r := range queued {
		if !r.Dirty {
			t.Fatal("confirmation cleared pending work")
		}
	}
}

func TestConfirmationRejectsInvalidAndCanceledRequests(t *testing.T) {
	db := openSyncTest(t)
	if err := db.Put([]Record{{Path: "file"}}, false); err != nil {
		t.Fatal(err)
	}
	for _, r := range []SyncConfirmation{{}, {Path: "../file", Generation: 1}, {All: true, Path: "file"}, {All: true, Generation: 1}} {
		if _, err := db.ConfirmSync(context.Background(), r, 10); err == nil {
			t.Fatal("invalid confirmation", r)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.ConfirmSync(ctx, SyncConfirmation{All: true}, 10); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	page, err := db.RequestedPage("", 10)
	if err != nil || len(page) != 0 {
		t.Fatal(page, err)
	}
}
