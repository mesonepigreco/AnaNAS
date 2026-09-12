package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

func TestExternalDirectoryCaptureRetainsIdentityAndAbortCharge(t *testing.T) {
	f := setup(t)
	if err := f.c.ConfigurePublicationBudget(journal.PublicationBudget{MaxBytes: 4 << 20, MaxEntries: 10}); err != nil {
		t.Fatal(err)
	}
	s, err := stage.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(f.root, "folder")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := content.Fingerprint(info)
	p := proposal(10, "folder", "", dirVersion(10))
	ctx := context.Background()
	interrupted := errors.New("receipt sealed before journal commit")
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
	// Child metadata can change after capture without replacing the directory.
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c, err = journal.Open(f.state, id(99))
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.c.ImportExternal(ctx, f.c.Epoch(), p, func(ctx context.Context, resume bool) (journal.Version, error) {
		return f.p.CaptureExternal(ctx, p.Entries[0], nil, s, resume)
	})
	if err != nil || !r.Committed || !r.Entries[0].Next.Directory {
		t.Fatal(r, err)
	}
	if match, err := f.p.MatchExternalDirectory(ctx, "folder", want, p.Entries[0].Next.ID); err != nil || !match {
		t.Fatal(match, err)
	}
	contents(t, filepath.Join(path, "child"), []byte("keep"))
	usage, err := f.c.PublicationBudgetUsage()
	if err != nil {
		t.Fatal(err)
	}
	abort := proposal(11, "folder", id(10), dirVersion(11))
	_, err = f.c.ImportExternal(ctx, f.c.Epoch(), abort, func(ctx context.Context, resume bool) (journal.Version, error) {
		v, e := f.p.CaptureExternal(ctx, abort.Entries[0], &want, s, resume)
		if e != nil {
			return v, e
		}
		return v, interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	if err := f.c.AbortExternal(ctx, f.c.Epoch(), abort.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.state, id(11)+".directory")); !os.IsNotExist(err) {
		t.Fatal("uncommitted receipt retained", err)
	}
	after, err := f.c.PublicationBudgetUsage()
	if err != nil || *after != *usage {
		t.Fatal("abort lost accounting", usage, after, err)
	}
	contents(t, filepath.Join(path, "child"), []byte("keep"))
}
