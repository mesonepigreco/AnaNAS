package index

import (
	"context"
	"errors"
	"testing"
)

func preparedFixture(t *testing.T, d *DB) (UploadPreparation, Upload) {
	t.Helper()
	u := uploadFixture(t, d)
	r, ok, err := d.Get("file")
	if err != nil || !ok {
		t.Fatal(r, err)
	}
	e := u.Proposal.Entries[0]
	p := UploadPreparation{Namespace: u.Namespace, ID: u.Proposal.ID, Client: u.Proposal.Client, Sources: []UploadSource{{Path: e.Path, Expected: e.Expected, ID: e.Next.ID, Generation: r.Generation, Fingerprint: r.Fingerprint}}}
	return p, u
}

func TestPreparationLocksCleanupAndAtomicallyPromotes(t *testing.T) {
	d := openSyncTest(t)
	p, u := preparedFixture(t, d)
	err := d.BuildUpload(context.Background(), p, func() error { return nil }, func() (Upload, error) {
		if pending, err := d.PendingUploadPreparation(); err != nil || pending == nil {
			t.Fatal("builder started without durable ownership", pending, err)
		}
		if err := d.AbortUploadPreparation(func(UploadPreparation) error { t.Fatal("aborted live builder"); return nil }); err == nil {
			t.Fatal("live builder lock ignored")
		}
		if err := d.PrepareUpload(u); err == nil {
			t.Fatal("direct preparation raced active builder")
		}
		if err := d.BuildUpload(context.Background(), p, func() error { t.Fatal("second builder claimed storage"); return nil }, func() (Upload, error) { return u, nil }); err == nil {
			t.Fatal("second builder accepted")
		}
		return u, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := d.PendingUploadPreparation(); err != nil || pending != nil {
		t.Fatal("promoted preparation retained", pending, err)
	}
	if pending, err := d.PendingUpload(); err != nil || pending == nil {
		t.Fatal("promoted outbox missing", pending, err)
	}
}

func TestFailedPreparationCannotPromoteAnotherIdentity(t *testing.T) {
	d := openSyncTest(t)
	p, u := preparedFixture(t, d)
	wrong := u
	wrong.Proposal.ID = version("b").Content.ID
	if err := d.BuildUpload(context.Background(), p, func() error { return nil }, func() (Upload, error) { return wrong, nil }); !errors.Is(err, ErrStale) {
		t.Fatal("unclaimed outbox accepted", err)
	}
	if err := d.PrepareUpload(u); !errors.Is(err, ErrStale) {
		t.Fatal("direct outbox bypassed preparation", err)
	}
	cleanupError := errors.New("directory flush failed")
	if err := d.AbortUploadPreparation(func(UploadPreparation) error { return cleanupError }); !errors.Is(err, cleanupError) {
		t.Fatal(err)
	}
	if pending, err := d.PendingUploadPreparation(); err != nil || pending == nil {
		t.Fatal("cleanup failure forgot provenance", pending, err)
	}
	if err := d.AbortUploadPreparation(func(UploadPreparation) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := d.PrepareUpload(u); err != nil {
		t.Fatal("abort did not release preparation", err)
	}
}
