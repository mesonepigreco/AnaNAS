package replica

import (
	"context"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/content"
	"testing"
)

func TestDownloadedProgressVerificationDoesNotAcknowledgeSameSizeEdit(t *testing.T) {
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
	w := &Worker{db: db, root: root, opts: WorkerOptions{PushOptions: completionOptions(), Ready: func() bool { return true }}}
	batch := &changefeed.Batch{Proposal: u.Proposal}
	observeFile(t, db, f.root, "file", []byte("uploaded snapshot"))
	w.verifyDownloaded(ctx, batch)
	r, _, _ := db.Get("file")
	if r.Dirty {
		t.Fatal("download-generated observation was not verified")
	}
	observeFile(t, db, f.root, "file", []byte("modified snapshot"))
	w.verifyDownloaded(ctx, batch)
	r, _, _ = db.Get("file")
	if !r.Dirty {
		t.Fatal("same-size local edit was marked synchronized")
	}
}
