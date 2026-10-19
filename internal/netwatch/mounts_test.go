package netwatch

import (
	"context"
	"testing"
	"time"
)

func TestMountWaitEndsOnCancellationWithoutPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- WaitMountChange(ctx) }()
	select {
	case err := <-done:
		t.Fatal("returned without a mount change", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not wake the wait")
	}
}
