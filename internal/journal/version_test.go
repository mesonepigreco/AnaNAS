package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestHistoricalVersionPathBindingAndRestart(t *testing.T) {
	c, dir := openTest(t)
	first := proposal(2, "folder/file", "")
	second := proposal(3, "folder/file", first.Entries[0].Next.ID)
	for _, p := range []Proposal{first, second} {
		if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	c, err := Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, p := range []Proposal{first, second} {
		v, pending, err := c.VersionAt("folder/file", p.Entries[0].Next.ID)
		if err != nil || pending || v == nil || *v != p.Entries[0].Next {
			t.Fatal(v, pending, err)
		}
		if _, _, err := c.VersionAt("other/file", p.Entries[0].Next.ID); !errors.Is(err, ErrIdentity) {
			t.Fatal("cross-path lookup accepted", err)
		}
	}
	for _, path := range []string{"", ".", "../file", ".nas-sync/file"} {
		if _, _, err := c.VersionAt(path, first.Entries[0].Next.ID); err == nil {
			t.Fatal("invalid path accepted", path)
		}
	}
}

func TestLegacyVersionOnlyAccessibleThroughCurrentHead(t *testing.T) {
	c, _ := openTest(t)
	first := proposal(2, "file", "")
	second := proposal(3, "file", first.Entries[0].Next.ID)
	for _, p := range []Proposal{first, second} {
		if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil {
			t.Fatal(err)
		}
		if err := c.db.Update(func(tx *bolt.Tx) error {
			raw, err := json.Marshal(p.Entries[0].Next)
			if err != nil {
				return err
			}
			return tx.Bucket(versionsBucket).Put([]byte(p.Entries[0].Next.ID), raw)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := c.VersionAt("file", first.Entries[0].Next.ID); !errors.Is(err, ErrIdentity) {
		t.Fatal("unbound historical access accepted", err)
	}
	v, pending, err := c.VersionAt("file", second.Entries[0].Next.ID)
	if err != nil || pending || v == nil || *v != second.Entries[0].Next {
		t.Fatal(v, pending, err)
	}
	if _, _, err := c.VersionAt("another", second.Entries[0].Next.ID); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
}

func TestPendingVersionNotExposed(t *testing.T) {
	c, _ := openTest(t)
	p := proposal(2, "file", "")
	if _, err := c.Commit(context.Background(), c.Epoch(), p, publishFunc(func(context.Context, Record) error { return errors.New("pending") })); err == nil {
		t.Fatal("expected publisher error")
	}
	if v, _, err := c.VersionAt("file", p.Entries[0].Next.ID); err == nil || v != nil {
		t.Fatal("prepared version exposed", v, err)
	}
}
