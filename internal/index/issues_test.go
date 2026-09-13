package index

import (
	"context"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
	"testing"
)

func TestUnsupportedAbsenceDoesNotClearRetainedHistory(t *testing.T) {
	db := openSyncTest(t)
	path := `folder/\`
	if err := db.Put([]Record{{Path: path, Missing: true}}, false); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range [][]byte{bases, pathStates} {
		if err := db.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucket).Put([]byte(path), []byte("retained")) }); err != nil {
			t.Fatal(err)
		}
		if cleared, err := db.ClearUnsupportedAbsence(path, 1); err != nil || cleared {
			t.Fatal(cleared, err)
		}
		if err := db.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucket).Delete([]byte(path)) }); err != nil {
			t.Fatal(err)
		}
	}
	if cleared, err := db.ClearUnsupportedAbsence(path, 1); err != nil || !cleared {
		t.Fatal(cleared, err)
	}
}

func TestPendingPaginationPastUnsupportedName(t *testing.T) {
	db := openSyncTest(t)
	records := make([]Record, 0, 201)
	for i := 0; i < 199; i++ {
		records = append(records, Record{Path: fmt.Sprintf("a-%03d", i)})
	}
	records = append(records, Record{Path: `m-\`}, Record{Path: "z-good"})
	if err := db.Put(records, false); err != nil {
		t.Fatal(err)
	}
	first, err := db.PendingSync(context.Background(), "", 10)
	if err != nil || first.Next != `m-\` || len(first.Items) != 200 {
		t.Fatal(first, err)
	}
	last, err := db.PendingSync(context.Background(), first.Next, 10)
	if err != nil || len(last.Items) != 1 || last.Items[0].Path != "z-good" || last.Next != "" || last.Total != 201 {
		t.Fatal(last, err)
	}
}

func TestUnsupportedPathsRemainPendingAndCannotBeConfirmed(t *testing.T) {
	db := openSyncTest(t)
	records := []Record{{Path: `folder/\`}, {Path: "symlink", Fingerprint: Fingerprint{Mode: uint32(os.ModeSymlink)}}, {Path: "good", Fingerprint: Fingerprint{Size: 1}}}
	if err := db.Put(records, false); err != nil {
		t.Fatal(err)
	}
	p, err := db.PendingSync(context.Background(), "", 10)
	if err != nil || len(p.Items) != 3 {
		t.Fatal(p, err)
	}
	for _, item := range p.Items {
		if item.Confirmable != (item.Path == "good") {
			t.Fatal(item)
		}
	}
	r, err := db.ConfirmSync(context.Background(), SyncConfirmation{All: true}, 10)
	if err != nil || r.Queued != 1 || r.Blocked != 2 {
		t.Fatal(r, err)
	}
}

func TestIssuesAreGenerationBoundAndManualRetryDoesNotAcknowledge(t *testing.T) {
	db := openSyncTest(t)
	ctx := context.Background()
	r := Record{Path: "file", Fingerprint: Fingerprint{Size: 1}}
	if err := db.Put([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordSyncIssue(r.Path, 1, "Permission denied", false); err != nil {
		t.Fatal(err)
	}
	p, err := db.PendingSync(ctx, "", 10)
	if err != nil || p.Items[0].Reason != "Permission denied" || !p.Items[0].Confirmable {
		t.Fatal(p, err)
	}
	if err := db.ClearTransientIssues(ctx); err != nil {
		t.Fatal(err)
	}
	r, _, _ = db.Get(r.Path)
	if issue, err := db.SyncIssue(r); err != nil || issue == nil {
		t.Fatal(issue, err)
	}
	if _, err := db.ConfirmSync(ctx, SyncConfirmation{Path: r.Path, Generation: 1}, 10); err != nil {
		t.Fatal(err)
	}
	if issue, err := db.SyncIssue(r); err != nil || issue != nil {
		t.Fatal(issue, err)
	}
	if err := db.RecordSyncIssue(r.Path, 1, "Changed", true); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearTransientIssues(ctx); err != nil {
		t.Fatal(err)
	}
	if issue, err := db.SyncIssue(r); err != nil || issue != nil {
		t.Fatal(issue, err)
	}
	if err := db.RecordSyncIssue(r.Path, 1, "Permission denied", false); err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]Record{r}, true); err != nil {
		t.Fatal(err)
	}
	r, _, _ = db.Get(r.Path)
	if issue, err := db.SyncIssue(r); err != nil || issue != nil || !r.Dirty {
		t.Fatal(issue, r, err)
	}
}
