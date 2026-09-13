package replica

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func TestDeletedUnsupportedNameLeavesQueueAndRecreationReturns(t *testing.T) {
	_, db, _ := preparationFixture(t)
	defer db.Close()
	r := index.Record{Path: `folder/\`, Missing: true}
	if err := db.Put([]index.Record{r, {Path: "good"}}, false); err != nil {
		t.Fatal(err)
	}
	w := &Worker{db: db, opts: WorkerOptions{PushOptions: PushOptions{MaxFileBytes: 10, MaxBatchBytes: 20}}}
	paths, _, err := w.selectLocal("")
	if err != nil || len(paths) != 1 || paths[0] != "good" {
		t.Fatal(paths, err)
	}
	p, err := db.PendingSync(context.Background(), "", 10)
	if err != nil || p.Total != 1 {
		t.Fatal(p, err)
	}
	r.Missing = false
	if err := db.Put([]index.Record{r}, false); err != nil {
		t.Fatal(err)
	}
	if cleared, err := db.ClearUnsupportedAbsence(r.Path, 1); err != nil || cleared {
		t.Fatal(cleared, err)
	}
	p, err = db.PendingSync(context.Background(), "", 10)
	if err != nil || p.Total != 2 {
		t.Fatal(p, err)
	}
	if cleared, err := db.ClearUnsupportedAbsence("good", 1); err != nil || cleared {
		t.Fatal(cleared, err)
	}
}

func TestConfirmedFileGetsPriorityDuringExistingPassWithoutSkippingParent(t *testing.T) {
	_, db, _ := preparationFixture(t)
	defer db.Close()
	if err := db.Put([]index.Record{{Path: "a-ordinary"}, {Path: "z-folder", Fingerprint: index.Fingerprint{Mode: uint32(os.ModeDir)}}, {Path: "z-folder/file"}}, false); err != nil {
		t.Fatal(err)
	}
	w := &Worker{db: db, opts: WorkerOptions{PushOptions: PushOptions{MaxFileBytes: 10, MaxBatchBytes: 20}}, wake: make(chan struct{}, 1)}
	pass := workerPass{requestsDone: true}
	if _, err := db.ConfirmSync(context.Background(), index.SyncConfirmation{Path: "z-folder/file", Generation: 1}, 10); err != nil {
		t.Fatal(err)
	}
	w.RequestSync()
	for _, want := range []string{"z-folder", "z-folder/file"} {
		paths, _, err := w.selectNextLocal(&pass)
		if err != nil || len(paths) != 1 || paths[0] != want {
			t.Fatal(paths, err, want)
		}
	}
	paths, more, err := w.selectNextLocal(&pass)
	if err != nil || len(paths) != 0 || !more {
		t.Fatal(paths, more, err)
	}
	paths, _, err = w.selectNextLocal(&pass)
	if err != nil || len(paths) == 0 || paths[0] != "a-ordinary" {
		t.Fatal("ordinary queue lost", paths, err)
	}
}

func TestOversizedFilesDoNotStarveLaterDirtyPages(t *testing.T) {
	_, db, _ := preparationFixture(t)
	defer db.Close()
	records := []index.Record{{Path: "0-small", Fingerprint: index.Fingerprint{Size: 1}}}
	for i := 0; i < journal.MaxEntries+2; i++ {
		records = append(records, index.Record{Path: fmt.Sprintf("a-big-%04d", i), Fingerprint: index.Fingerprint{Size: 11}})
	}
	records = append(records, index.Record{Path: "z-small", Fingerprint: index.Fingerprint{Size: 1}})
	if err := db.Put(records, false); err != nil {
		t.Fatal(err)
	}
	w := &Worker{db: db, opts: WorkerOptions{PushOptions: PushOptions{MaxFileBytes: 10, MaxBatchBytes: 20}}}
	var selected []string
	cursor := ""
	for i := 0; ; i++ {
		if i > 5 {
			t.Fatal("selection did not terminate")
		}
		paths, next, err := w.selectLocal(cursor)
		if err != nil && !errors.Is(err, ErrAttention) {
			t.Fatal(err)
		}
		selected = append(selected, paths...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(selected) != 2 || selected[0] != "0-small" || selected[1] != "z-small" {
		t.Fatal(selected)
	}
	r, _, err := db.Get("a-big-0000")
	if err != nil || !r.Dirty || r.Excluded {
		t.Fatal("oversized work lost", r, err)
	}
	// Raising the limit makes the retained work eligible without a new event.
	w.opts.MaxFileBytes = 20
	paths, _, err := w.selectLocal("0-small")
	if err != nil || len(paths) != 1 || paths[0] != "a-big-0000" {
		t.Fatal(paths, err)
	}
}

func TestMultiGiBSelectionAndBatchBoundaries(t *testing.T) {
	_, db, _ := preparationFixture(t)
	defer db.Close()
	records := []index.Record{
		{Path: "a", Fingerprint: index.Fingerprint{Size: 3<<30 + 17}},
		{Path: "b", Fingerprint: index.Fingerprint{Size: 8 << 30}},
		{Path: "c", Fingerprint: index.Fingerprint{Size: 8<<30 + 1}},
		{Path: "d", Fingerprint: index.Fingerprint{Size: 1}},
	}
	if err := db.Put(records, false); err != nil {
		t.Fatal(err)
	}
	w := &Worker{db: db, opts: WorkerOptions{PushOptions: PushOptions{MaxFileBytes: 8 << 30, MaxBatchBytes: 8 << 30}}}
	cursor := ""
	for _, want := range []string{"a", "b", "d"} {
		paths, next, err := w.selectLocal(cursor)
		if err != nil && !errors.Is(err, ErrAttention) {
			t.Fatal(err)
		}
		if len(paths) != 1 || paths[0] != want {
			t.Fatal(paths, want)
		}
		cursor = next
	}
}

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
