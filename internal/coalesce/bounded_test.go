package coalesce

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSlowConsumerDoesNotBlockInputAndOverflowRequestsRescan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	in := make(chan string)
	out := New(Config{Idle: time.Millisecond, MaxWait: 5 * time.Millisecond, MaxPending: 4, MaxBytes: 64}).Run(ctx, in)
	// Intentionally do not read output until input has been fully consumed.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		defer close(in)
		for i := 0; i < 1000; i++ {
			select {
			case in <- fmt.Sprint(i):
			case <-ctx.Done():
				return
			}
		}
	}()
	select {
	case <-sent:
	case <-ctx.Done():
		t.Fatal("slow consumer blocked input")
	}
	rescan := false
	for b := range out {
		rescan = rescan || b.Rescan
		if len(b.Paths) > 4 {
			t.Fatal("unbounded batch")
		}
	}
	if !rescan {
		t.Fatal("overflow lost without rescan")
	}
}
func TestCancellationWhileOutputBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan string, 1)
	in <- "a"
	out := New(Config{Idle: time.Millisecond, MaxWait: time.Millisecond}).Run(ctx, in)
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			for range out {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
}
func TestByteOverflow(t *testing.T) {
	in := make(chan string, 1)
	in <- "too-long"
	close(in)
	out := New(Config{Idle: time.Millisecond, MaxWait: time.Millisecond, MaxBytes: 2}).Run(context.Background(), in)
	b := <-out
	if !b.Rescan || len(b.Paths) != 0 {
		t.Fatalf("%+v", b)
	}
}
func TestExplicitGap(t *testing.T) {
	in := make(chan string, 2)
	in <- "file"
	in <- RescanPath
	close(in)
	b := <-New(Config{Idle: time.Second, MaxWait: time.Second}).Run(context.Background(), in)
	if !b.Rescan || len(b.Paths) != 0 {
		t.Fatalf("%+v", b)
	}
}
