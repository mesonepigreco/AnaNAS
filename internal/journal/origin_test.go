package journal

import (
	"context"
	"testing"
)

func TestReplicaOriginPersistsAndRejectsUnboundHistory(t *testing.T) {
	c, dir := openTest(t)
	if err := c.BindReplicaOrigin(id(7)); err != nil {
		t.Fatal(err)
	}
	if err := c.BindReplicaOrigin(id(7)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err := Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.BindReplicaOrigin(id(8)); err == nil {
		t.Fatal("replica target changed after restart")
	}
	if err := c.BindReplicaOrigin(id(7)); err != nil {
		t.Fatal(err)
	}
	unbound, _ := openTest(t)
	if _, err := unbound.Commit(context.Background(), unbound.Epoch(), proposal(2, "file", ""), success); err != nil {
		t.Fatal(err)
	}
	if err := unbound.BindReplicaOrigin(id(7)); err == nil {
		t.Fatal("unbound history silently assigned an origin")
	}
}
