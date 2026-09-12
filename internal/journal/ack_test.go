package journal

import (
	"context"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestAcknowledgementAcceptsOneFullPage(t *testing.T) {
	c, _ := openTest(t)
	for n := 2; n < MaxPage+2; n++ {
		if _, err := c.Commit(context.Background(), c.Epoch(), proposal(n, id(n), ""), success); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.AcknowledgeRange(id(70), id(80), 0, MaxPage); err != nil {
		t.Fatal(err)
	}
	if cursor, err := c.Cursor(id(70)); err != nil || cursor != MaxPage {
		t.Fatal(cursor, err)
	}
}

func TestBoundAcknowledgementRangeRestartAndIdempotence(t *testing.T) {
	c, dir := openTest(t)
	for n := 2; n < 5; n++ {
		if _, err := c.Commit(context.Background(), c.Epoch(), proposal(n, id(n), ""), success); err != nil {
			t.Fatal(err)
		}
	}
	client, policy := id(70), id(80)
	if err := c.AcknowledgeRange(client, policy, 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatal("skipped prefix", err)
	}
	if err := c.AcknowledgeRange(client, policy, 0, 4); err == nil {
		t.Fatal("future sequence acknowledged")
	}
	if cursor, err := c.Cursor(client); err != nil || cursor != 0 {
		t.Fatal(cursor, err)
	}
	if err := c.AcknowledgeRange(client, policy, 0, 2); err != nil {
		t.Fatal(err)
	}
	c.Close()
	var err error
	c, err = Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writes := c.db.Stats().TxStats.Write
	if err := c.AcknowledgeRange(client, policy, 0, 2); err != nil {
		t.Fatal("lost reply not retryable", err)
	}
	if c.db.Stats().TxStats.Write != writes {
		t.Fatal("identical retry wrote database")
	}
	if err := c.AcknowledgeRange(client, id(81), 2, 3); err == nil {
		t.Fatal("changed policy continued cursor")
	}
	if err := c.Acknowledge(client, 2, 3); err == nil {
		t.Fatal("legacy method bypassed policy")
	}
	if err := c.AcknowledgeRange(client, policy, 2, 3); err != nil {
		t.Fatal(err)
	}
	if cursor, err := c.Cursor(client); err != nil || cursor != 3 {
		t.Fatal(cursor, err)
	}
	if cursor, err := c.Cursor(id(71)); err != nil || cursor != 0 {
		t.Fatal("another client advanced", cursor, err)
	}
}

func TestAcknowledgementRefusesGapsLegacyAndExcessClients(t *testing.T) {
	c, _ := openTest(t)
	for n := 2; n < 5; n++ {
		if _, err := c.Commit(context.Background(), c.Epoch(), proposal(n, id(n), ""), success); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Acknowledge(id(66), 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.AcknowledgeRange(id(66), id(80), 1, 2); err == nil {
		t.Fatal("adopted unbound legacy cursor")
	}
	if err := c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(changesBucket).Delete(key(2)) }); err != nil {
		t.Fatal(err)
	}
	if err := c.AcknowledgeRange(id(67), id(80), 0, 3); err == nil {
		t.Fatal("missing middle record skipped")
	}
	if cursor, err := c.Cursor(id(67)); err != nil || cursor != 0 {
		t.Fatal("partial range advanced", cursor, err)
	}
	for n := 100; n < 164; n++ {
		if err := c.AcknowledgeRange(id(n), id(80), 0, 1); err != nil {
			t.Fatal(n, err)
		}
	}
	if err := c.AcknowledgeRange(id(164), id(80), 0, 1); err == nil {
		t.Fatal("client table unbounded")
	}
	if err := c.AcknowledgeRange(id(100), id(80), 0, 1); err != nil {
		t.Fatal("full table refused existing client", err)
	}
	for _, r := range [][2]uint64{{0, 0}, {0, MaxPage + 1}, {^uint64(0), 0}} {
		if err := c.AcknowledgeRange(id(68), id(80), r[0], r[1]); err == nil {
			t.Fatal("invalid range accepted", r)
		}
	}
}
