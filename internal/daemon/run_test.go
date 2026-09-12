package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

type testObserver struct {
	changes, reconciled, started, stopped chan struct{}
	fail                                  error
}

func (o *testObserver) Run(ctx context.Context) error {
	close(o.started)
	defer close(o.stopped)
	if o.fail != nil {
		return o.fail
	}
	<-ctx.Done()
	return nil
}
func (o *testObserver) Changes() <-chan struct{}    { return o.changes }
func (o *testObserver) Reconciled() <-chan struct{} { return o.reconciled }

type testWorker struct{ changes, notified, interrupted, started, stopped chan struct{} }

func (w *testWorker) Run(ctx context.Context) error {
	close(w.started)
	<-ctx.Done()
	close(w.stopped)
	return nil
}
func (w *testWorker) Changes() <-chan struct{} { return w.changes }
func (w *testWorker) Notify()                  { w.notified <- struct{}{} }
func (w *testWorker) Interrupt()               { w.interrupted <- struct{}{} }

func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("service event missing")
	}
}

func TestRunSeparatesDisplayAndWorkHintsAndJoinsServices(t *testing.T) {
	o := &testObserver{changes: make(chan struct{}, 1), reconciled: make(chan struct{}, 1), started: make(chan struct{}), stopped: make(chan struct{})}
	w := &testWorker{changes: make(chan struct{}, 1), notified: make(chan struct{}, 1), interrupted: make(chan struct{}, 1), started: make(chan struct{}), stopped: make(chan struct{})}
	controls, displayed := make(chan struct{}, 1), make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o, controls, w, Surface{Notify: func() { displayed <- struct{}{} }}) }()
	await(t, o.started)
	await(t, w.started)
	o.changes <- struct{}{}
	await(t, displayed)
	select {
	case <-w.notified:
		t.Fatal("UI update woke transfer worker")
	default:
	}
	o.reconciled <- struct{}{}
	await(t, w.notified)
	controls <- struct{}{}
	await(t, w.interrupted)
	await(t, displayed)
	w.changes <- struct{}{}
	await(t, displayed)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not join services")
	}
	await(t, o.stopped)
	await(t, w.stopped)
}

func TestRunPreservesObserverFailureAndNilWorker(t *testing.T) {
	want := errors.New("inaccessible local root")
	o := &testObserver{changes: make(chan struct{}), reconciled: make(chan struct{}), started: make(chan struct{}), stopped: make(chan struct{}), fail: want}
	if err := Run(context.Background(), o, nil, nil, Surface{}); !errors.Is(err, want) {
		t.Fatal("observer failure lost", err)
	}
	await(t, o.stopped)
}

type testRemote struct {
	started, stopped chan struct{}
	failure          chan error
}

func (r *testRemote) WatchChanges(ctx context.Context, hints chan<- struct{}) error {
	close(r.started)
	defer close(r.stopped)
	hints <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-r.failure:
		return err
	}
}

func TestRemoteFailureStopsAndJoinsDaemonWithoutRetry(t *testing.T) {
	o := &testObserver{changes: make(chan struct{}), reconciled: make(chan struct{}), started: make(chan struct{}), stopped: make(chan struct{})}
	w := &testWorker{changes: make(chan struct{}), notified: make(chan struct{}, 1), interrupted: make(chan struct{}, 1), started: make(chan struct{}), stopped: make(chan struct{})}
	r := &testRemote{started: make(chan struct{}), stopped: make(chan struct{}), failure: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunWithRemote(ctx, o, nil, w, Surface{}, r) }()
	await(t, r.started)
	await(t, w.notified)
	want := errors.New("notification connection lost")
	r.failure <- want
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatal("stream error lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote failure did not stop lifecycle")
	}
	await(t, o.stopped)
	await(t, w.stopped)
	await(t, r.stopped)
}
