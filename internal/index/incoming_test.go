package index

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"nas-sync/internal/journal"
)

func TestIncomingChoicesPersistAndRejectStaleRequests(t *testing.T) {
	file := filepath.Join(t.TempDir(), "index.db")
	d, err := Open(file, "/root")
	if err != nil {
		t.Fatal(err)
	}
	v := journal.Version{ID: strings.Repeat("a", 64), Directory: true}
	e := journal.Entry{Path: "folder", Next: v}
	if err := d.RecordIncoming(e, "missing identity"); err != nil {
		t.Fatal(err)
	}
	if err := d.ChooseIncoming(IncomingChoice{Path: "folder", Revision: strings.Repeat("b", 64), Choice: "nas"}, strings.Repeat("c", 64)); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	for _, name := range []string{"folder/already-a", "folder/already-b"} {
		if err := d.RecordIncoming(journal.Entry{Path: name, Next: v}, "existing blocked child"); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ChooseIncoming(IncomingChoice{Path: "folder", Revision: v.ID, Choice: "separate"}, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetNotifySyncProblems(false); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(file, "/root")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.NotifySyncProblems(true) {
		t.Fatal("notification preference not retained")
	}
	e.Path = "folder/child"
	e.Next.ID = strings.Repeat("d", 64)
	if err := d.RecordIncoming(e, "parent kept separate"); err != nil {
		t.Fatal(err)
	}
	page, err := d.IncomingProblems(context.Background(), "")
	if err != nil || page.Total != 4 || page.Attention != 0 {
		t.Fatal(page, err)
	}
	if blocked, _ := d.IncomingBlocked("unrelated"); blocked {
		t.Fatal("unrelated path blocked")
	}
	if blocked, _ := d.IncomingBlocked("folder/new"); !blocked {
		t.Fatal("separate subtree resumed")
	}
	if err := d.ChooseIncoming(IncomingChoice{Path: "folder", Revision: v.ID, Choice: "nas"}, strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	if err := d.ChooseIncoming(IncomingChoice{Path: e.Path, Revision: e.Next.ID, Choice: "retry"}, strings.Repeat("f", 64)); !errors.Is(err, ErrStale) {
		t.Fatal("overlapping recovery accepted", err)
	}
	request, _ := d.NextIncomingRequest()
	if err := d.FinishIncomingRequest(*request, "", true); err != nil {
		t.Fatal(err)
	}
	page, _ = d.IncomingProblems(context.Background(), "")
	if page.Total != 0 {
		t.Fatal("subtree records skipped", page)
	}
}

func TestPublishedObservationDoesNotOverwriteConcurrentEdit(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "index.db"), "/root")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Put([]Record{{Path: "file", Fingerprint: Fingerprint{Inode: 22, Size: 9}}}, false); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsurePublishedObservation(Record{Path: "file", Missing: true}); err != nil {
		t.Fatal(err)
	}
	r, _, _ := d.Get("file")
	if r.Missing || r.Fingerprint.Inode != 22 {
		t.Fatal("new observation overwritten", r)
	}
	if err := d.EnsurePublishedObservation(Record{Path: "renamed-before-scan", Missing: true}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = d.Get("renamed-before-scan")
	if !r.Missing || !r.Dirty || r.Generation != 1 {
		t.Fatal(r)
	}
}
