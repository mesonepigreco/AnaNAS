package replica

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"nas-sync/internal/journal"
)

func TestContentTimeoutScalesAndWorkerRetriesOnlyRecoverableErrors(t *testing.T) {
	if got := contentTimeout(128<<20, 20<<20); got != 9*time.Minute+17*time.Second {
		t.Fatalf("production batch timeout = %v", got)
	}
	if got := contentTimeout(64<<20, 1<<20); got != 9*time.Minute+17*time.Second {
		t.Fatalf("paced batch timeout = %v", got)
	}
	if !retryableWorkerError(context.DeadlineExceeded) {
		t.Fatal("recoverable work was not retried")
	}
	if retryableWorkerError(fmt.Errorf("content invariant failed")) {
		t.Fatal("generic content failure was retried")
	}
	for _, err := range []error{ErrSuspended, ErrAttention, journal.ErrBudget, journal.ErrConflict, journal.ErrIdentity} {
		if retryableWorkerError(err) {
			t.Fatalf("non-recoverable error was retried: %v", err)
		}
	}
	for _, err := range []error{syscall.ENOENT, syscall.EACCES, fmt.Errorf("publish file: %w", syscall.ENOENT)} {
		if retryableWorkerError(err) {
			t.Fatalf("permanent filesystem error retried: %v", err)
		}
	}
	if !retryableWorkerError(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}) {
		t.Fatal("connection failure no longer retried")
	}
}
