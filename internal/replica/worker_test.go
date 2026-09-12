package replica

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/journal"
)

type quietWorkerRemote struct {
	WorkerRemote // All unimplemented transfer methods must remain unreachable.
	feeds        atomic.Int64
	fail         error
}

type blockedWorkerRemote struct {
	quietWorkerRemote
	started, canceled chan struct{}
}

func (r *blockedWorkerRemote) ChangesPage(ctx context.Context, _, _ string, _ uint64, _ int) (changefeed.Page, error) {
	r.feeds.Add(1)
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	return changefeed.Page{}, ctx.Err()
}

func TestWorkerPauseCancelsActiveRequestAndWaits(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 256 << 20, MaxEntries: 128}, id("remote namespace")); err != nil {
		t.Fatal(err)
	}
	remote := &blockedWorkerRemote{started: make(chan struct{}), canceled: make(chan struct{})}
	w, err := NewWorker(db, root, f.c, f.store, f.publisher, remote, WorkerOptions{Namespace: id("remote namespace"), PushOptions: completionOptions(), Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-remote.started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	if err := db.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	w.Interrupt()
	select {
	case <-remote.canceled:
	case <-time.After(time.Second):
		t.Fatal("pause did not cancel active request")
	}
	waitWorkerPhase(t, w, "suspended")
	time.Sleep(30 * time.Millisecond)
	if remote.feeds.Load() != 1 {
		t.Fatal("paused worker retried request")
	}
}

func (r *quietWorkerRemote) ChangesPage(context.Context, string, string, uint64, int) (changefeed.Page, error) {
	r.feeds.Add(1)
	return changefeed.Page{Namespace: id("remote namespace"), Policy: id("empty policy")}, r.fail
}

func waitWorkerPhase(t *testing.T, w *Worker, phase string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if w.Status().Phase == phase {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("missing phase", phase, w.Status())
}

func TestWorkerWaitsOnUnavailablePolicyAndDoesNotRetryContentErrors(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "idle"
		if failure {
			name = "feed failure"
		}
		t.Run(name, func(t *testing.T) {
			f, db, root := preparationFixture(t)
			defer db.Close()
			if err := f.c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 256 << 20, MaxEntries: 128}, id("remote namespace")); err != nil {
				t.Fatal(err)
			}
			remote := &quietWorkerRemote{}
			if failure {
				remote.fail = errors.New("NAS unavailable")
			}
			var ready atomic.Bool
			w, err := NewWorker(db, root, f.c, f.store, f.publisher, remote, WorkerOptions{Namespace: id("remote namespace"), PushOptions: completionOptions(), Ready: ready.Load})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- w.Run(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			waitWorkerPhase(t, w, "suspended")
			if remote.feeds.Load() != 0 {
				t.Fatal("unready worker contacted remote")
			}
			ready.Store(true)
			w.Notify()
			phase := "idle"
			if failure {
				phase = "attention"
			}
			waitWorkerPhase(t, w, phase)
			if remote.feeds.Load() != 1 {
				t.Fatal("unexpected feed requests", remote.feeds.Load())
			}
			time.Sleep(50 * time.Millisecond)
			if remote.feeds.Load() != 1 {
				t.Fatal("idle/retry path polled NAS without backoff", remote.feeds.Load())
			}
		})
	}
}
