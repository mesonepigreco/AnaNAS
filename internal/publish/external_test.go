package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

func TestExternalCaptureCommitsWithoutRewritingVisibleFile(t *testing.T) {
	f := setup(t)
	if err := f.c.ConfigurePublicationBudget(journal.PublicationBudget{MaxBytes: 4 << 20, MaxEntries: 10}); err != nil {
		t.Fatal(err)
	}
	s, err := stage.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(f.root, "file")
	data := []byte("edited through a native NAS application")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := content.Fingerprint(info)
	p := proposal(1, "file", "", journal.Version{ID: id(1), Size: int64(len(data)), Digest: hash.SumBytes(nil)})
	ctx := context.Background()
	r, err := f.c.ImportExternal(ctx, f.c.Epoch(), p, func(ctx context.Context, resume bool) (journal.Version, error) {
		return f.p.CaptureExternal(ctx, p.Entries[0], &want, s, resume)
	})
	if err != nil || !r.Committed || r.Entries[0].Next.Digest != hash.SumBytes(data) {
		t.Fatal(r, err)
	}
	after, err := os.Stat(path)
	if err != nil || content.Fingerprint(after) != want {
		t.Fatal("import rewrote source", err)
	}
	contents(t, path, data)
	contents(t, filepath.Join(f.state, id(1)+".ready"), data)
	before, _ := f.c.PublicationBudgetUsage()
	if _, err := f.c.ImportExternal(ctx, f.c.Epoch(), p, func(context.Context, bool) (journal.Version, error) {
		t.Fatal("committed retry recaptured data")
		return journal.Version{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	afterBudget, _ := f.c.PublicationBudgetUsage()
	if *before != *afterBudget {
		t.Fatal("retry charged twice")
	}
	// The normal publisher can use the imported immutable version as its base.
	updated := []byte("new synchronized content")
	commit(t, f, 2, "file", id(1), candidate(t, f.state, 2, updated))
	contents(t, path, updated)
}

func TestExternalCaptureResumePreservesLaterEditAndAbortReleasesReservation(t *testing.T) {
	f := setup(t)
	if err := f.c.ConfigurePublicationBudget(journal.PublicationBudget{MaxBytes: 4 << 20, MaxEntries: 10}); err != nil {
		t.Fatal(err)
	}
	s, err := stage.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(f.root, "file")
	data := []byte("captured before interruption")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	want := content.Fingerprint(info)
	p := proposal(3, "file", "", journal.Version{ID: id(3), Size: int64(len(data)), Digest: hash.SumBytes(nil)})
	ctx := context.Background()
	interrupted := errors.New("capture completed before journal commit")
	_, err = f.c.ImportExternal(ctx, f.c.Epoch(), p, func(ctx context.Context, resume bool) (journal.Version, error) {
		v, err := f.p.CaptureExternal(ctx, p.Entries[0], &want, s, resume)
		if err != nil {
			return v, err
		}
		return v, interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("later visible edit"), 0600); err != nil {
		t.Fatal(err)
	}
	// Reopen the real database to recover its durable local-intake marker.
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c, err = journal.Open(f.state, id(99))
	if err != nil {
		t.Fatal(err)
	}
	prior, err := f.c.PendingExternal()
	if err != nil || prior == nil {
		t.Fatal(prior, err)
	}
	r, err := f.c.ImportExternal(ctx, f.c.Epoch(), *prior, func(ctx context.Context, resume bool) (journal.Version, error) {
		return f.p.CaptureExternal(ctx, prior.Entries[0], nil, s, resume)
	})
	if err != nil || r.Entries[0].Next.Digest != hash.SumBytes(data) {
		t.Fatal(r, err)
	}
	contents(t, path, []byte("later visible edit"))
	before, _ := f.c.PublicationBudgetUsage()
	bad := proposal(4, "file", id(3), journal.Version{ID: id(4), Size: 50, Digest: hash.SumBytes(nil)})
	_, err = f.c.ImportExternal(ctx, f.c.Epoch(), bad, func(context.Context, bool) (journal.Version, error) { return journal.Version{}, interrupted })
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	if err := f.c.AbortExternal(ctx, f.c.Epoch(), bad.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := f.c.PublicationBudgetUsage()
	if *before != *after {
		t.Fatal("aborted capture did not release reservation", before, after)
	}
	if err := f.c.AbortExternal(ctx, f.c.Epoch(), p.ID); err == nil {
		t.Fatal("committed import could be aborted")
	}
}
