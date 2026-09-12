package index

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestRemoteAcknowledgementRequiresDurableCompletion(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "index.db")
	d, err := Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	p := remotePage(t, true)
	if err := d.ReceiveRemotePage(p); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRemoteAcknowledged(p.Namespace, p.Policy, 0, 1); !errors.Is(err, ErrStale) {
		t.Fatal("received-only batch acknowledged", err)
	}
	if err := d.CompleteRemoteBatch(1, p.Batches[0].ID); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(filename, "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := d.RemoteState()
	if err != nil || s.Completed != 1 || s.Acknowledged != 0 {
		t.Fatal("completion invented remote receipt", s, err)
	}
	if err := d.RecordRemoteAcknowledged(p.Namespace, p.Policy, 0, 2); !errors.Is(err, ErrStale) {
		t.Fatal("future reply accepted", err)
	}
	if err := d.RecordRemoteAcknowledged(p.Namespace, p.Policy, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRemoteAcknowledged(p.Namespace, p.Policy, 0, 1); err != nil {
		t.Fatal("duplicate reply refused", err)
	}
	s, err = d.RemoteState()
	if err != nil || s.Acknowledged != 1 || s.Completed != 1 {
		t.Fatal(s, err)
	}
}
