package journal

import (
	"context"
	"errors"
	"testing"
)

func TestObserveSignalsOnlyDurableCommitsAndClose(t *testing.T) {
	c, dir := openTest(t)
	sequence, initial, err := c.Observe()
	if err != nil || sequence != 0 {
		t.Fatal(sequence, err)
	}
	_, other, err := c.Observe()
	if err != nil || other != initial {
		t.Fatal("readers did not share bounded signal", err)
	}
	p := proposal(2, "file", "")
	want := errors.New("publication interrupted")
	if _, err := c.Commit(context.Background(), c.Epoch(), p, publishFunc(func(context.Context, Record) error { return want })); !errors.Is(err, want) {
		t.Fatal(err)
	}
	select {
	case <-initial:
		t.Fatal("uncommitted publication notified readers")
	default:
	}
	if _, err := c.Recover(context.Background(), c.Epoch(), success); err != nil {
		t.Fatal(err)
	}
	select {
	case <-initial:
	default:
		t.Fatal("recovered durable commit not signaled")
	}
	sequence, next, err := c.Observe()
	if err != nil || sequence != 1 || next == initial {
		t.Fatal(sequence, err)
	}
	if _, err := c.Commit(context.Background(), c.Epoch(), p, success); err != nil {
		t.Fatal(err)
	}
	select {
	case <-next:
		t.Fatal("idempotent retry emitted redundant event")
	default:
	}
	// A slow reader retains one closed signal across many commits; it need not
	// drain a queue or read every intermediate sequence.
	for n := 3; n < 20; n++ {
		if _, err := c.Commit(context.Background(), c.Epoch(), proposal(n, id(n), ""), success); err != nil {
			t.Fatal(err)
		}
	}
	sequence, closed, err := c.Observe()
	if err != nil || sequence != 18 {
		t.Fatal(sequence, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("close did not wake readers")
	}
	if _, _, err := c.Observe(); err == nil {
		t.Fatal("closed observation succeeded")
	}
	reopened, err := Open(dir, id(99))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	sequence, _, err = reopened.Observe()
	if err != nil || sequence != 18 {
		t.Fatal("restart lost high water", sequence, err)
	}
}
