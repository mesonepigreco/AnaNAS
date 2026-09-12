package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type sessionNetwork struct {
	change           chan error
	started, stopped chan struct{}
	subscribe        <-chan struct{}
	fail             error
}

func (n *sessionNetwork) Watch(ctx context.Context, ready chan<- struct{}) error {
	n.started <- struct{}{}
	defer func() { n.stopped <- struct{}{} }()
	if n.fail != nil {
		return n.fail
	}
	if n.subscribe != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-n.subscribe:
		}
	}
	ready <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-n.change:
		return err
	}
}

type sessionWorker struct {
	started, stopped, notified, interrupted, changes chan struct{}
	release                                          <-chan struct{}
	fail                                             error
}

func (w *sessionWorker) Run(ctx context.Context) error {
	w.started <- struct{}{}
	defer func() { w.stopped <- struct{}{} }()
	if w.fail != nil {
		return w.fail
	}
	<-ctx.Done()
	if w.release != nil {
		<-w.release
	}
	return nil
}
func (w *sessionWorker) Notify() {
	select {
	case w.notified <- struct{}{}:
	default:
	}
}
func (w *sessionWorker) Interrupt() {
	select {
	case w.interrupted <- struct{}{}:
	default:
	}
}
func (w *sessionWorker) Changes() <-chan struct{} { return w.changes }

type sessionRemote struct {
	started, stopped chan struct{}
	fail             chan error
}

func (r *sessionRemote) Interrupt() {} // fixture owns no pooled sockets

func (r *sessionRemote) WatchChanges(ctx context.Context, hints chan<- struct{}) error {
	r.started <- struct{}{}
	defer func() { r.stopped <- struct{}{} }()
	hints <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-r.fail:
		return err
	}
}

func sessionFixtures() (*testObserver, *sessionWorker, *sessionRemote, *sessionNetwork) {
	c := func() chan struct{} { return make(chan struct{}, 8) }
	return &testObserver{started: c(), stopped: c(), changes: c(), reconciled: c()},
		&sessionWorker{started: c(), stopped: c(), notified: c(), interrupted: c(), changes: c()},
		&sessionRemote{started: c(), stopped: c(), fail: make(chan error, 1)},
		&sessionNetwork{started: c(), stopped: c(), change: make(chan error, 1)}
}

func sessionAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(4 * time.Second):
		t.Fatal("session event missing")
	}
}
func sessionQuiet(t *testing.T, ch <-chan struct{}, duration time.Duration) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected session event")
	case <-time.After(duration):
	}
}
func sessionFinish(t *testing.T, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle did not join")
	}
}

func TestNetworkSessionsPreserveObserverAndControls(t *testing.T) {
	o, w, r, n := sessionFixtures()
	subscribe := make(chan struct{})
	n.subscribe = subscribe
	var checks atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controls, displayed := make(chan struct{}, 1), make(chan struct{}, 8)
	done := make(chan error, 1)
	go func() {
		done <- RunWithNetwork(ctx, o, controls, w, Surface{Notify: func() { displayed <- struct{}{} }}, r, NetworkOptions{n, func(context.Context) error { checks.Add(1); return nil }})
	}()
	await(t, o.started)
	await(t, n.started)
	sessionQuiet(t, w.started, 20*time.Millisecond)
	if checks.Load() != 0 {
		t.Fatal("check preceded subscription")
	}
	close(subscribe)
	await(t, w.started)
	await(t, r.started)
	await(t, w.notified)
	n.change <- errors.New("route removed")
	await(t, w.stopped)
	await(t, r.stopped)
	await(t, n.stopped)
	sessionQuiet(t, o.stopped, 20*time.Millisecond)
	// Local UI and durable controls remain responsive during reconnect backoff.
	o.changes <- struct{}{}
	await(t, displayed)
	controls <- struct{}{}
	await(t, w.interrupted)
	await(t, displayed)
	sessionAwait(t, w.started)
	sessionAwait(t, r.started)
	if checks.Load() != 2 {
		t.Fatal("reconnect did not revalidate", checks.Load())
	}
	// NAS restart/stream EOF also reconnects without a PC route event.
	r.fail <- errors.New("NAS restarted")
	await(t, w.stopped)
	await(t, r.stopped)
	sessionAwait(t, w.started)
	sessionAwait(t, r.started)
	if checks.Load() != 3 {
		t.Fatal("stream recovery did not revalidate")
	}
	sessionQuiet(t, o.stopped, 20*time.Millisecond)
	sessionFinish(t, done, cancel)
	await(t, o.stopped)
	await(t, w.stopped)
	await(t, r.stopped)
}

func TestNetworkDeniedPolicyHasNoRetryAndKeepsObservation(t *testing.T) {
	o, w, r, n := sessionFixtures()
	var allowed atomic.Bool
	var checks atomic.Int32
	offline := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunWithNetwork(ctx, o, nil, w, Surface{Network: func(s NetworkStatus) {
			if s.Phase == "offline" {
				offline <- struct{}{}
			}
		}}, r, NetworkOptions{n, func(context.Context) error {
			checks.Add(1)
			if !allowed.Load() {
				return errors.New("no direct LAN")
			}
			return nil
		}})
	}()
	await(t, o.started)
	await(t, offline)
	sessionQuiet(t, w.started, 1100*time.Millisecond)
	if checks.Load() != 1 {
		t.Fatal("offline policy polled", checks.Load())
	}
	sessionQuiet(t, r.started, 10*time.Millisecond)
	allowed.Store(true)
	n.change <- errors.New("LAN restored")
	sessionAwait(t, w.started)
	sessionAwait(t, r.started)
	sessionQuiet(t, o.stopped, 10*time.Millisecond)
	sessionFinish(t, done, cancel)
}

func TestNetworkRecoveryJoinsBeforeRestart(t *testing.T) {
	o, w, r, n := sessionFixtures()
	release := make(chan struct{})
	w.release = release
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunWithNetwork(ctx, o, nil, w, Surface{}, r, NetworkOptions{n, func(context.Context) error { return nil }})
	}()
	await(t, w.started)
	await(t, r.started)
	n.change <- errors.New("changed")
	await(t, r.stopped)
	// Even after the retry interval, no session can reuse the still-stopping worker.
	sessionQuiet(t, r.started, 1100*time.Millisecond)
	close(release)
	await(t, w.stopped)
	sessionAwait(t, r.started)
	sessionFinish(t, done, cancel)
}

func TestNetworkPendingSubscriptionAndRetryCancellation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "retrying"}[fail], func(t *testing.T) {
			o, w, r, n := sessionFixtures()
			n.subscribe = make(chan struct{})
			if fail {
				n.fail = errors.New("subscription denied")
			}
			retrying := make(chan struct{}, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- RunWithNetwork(ctx, o, nil, w, Surface{Network: func(s NetworkStatus) {
					if s.Phase == "retrying" {
						retrying <- struct{}{}
					}
				}}, r, NetworkOptions{n, func(context.Context) error { t.Error("unexpected policy check"); return nil }})
			}()
			await(t, o.started)
			await(t, n.started)
			if fail {
				await(t, retrying)
			}
			sessionFinish(t, done, cancel)
			await(t, o.stopped)
			await(t, n.stopped)
			sessionQuiet(t, w.started, 10*time.Millisecond)
		})
	}
}

func TestNetworkWorkerFailureRemainsFatal(t *testing.T) {
	o, w, r, n := sessionFixtures()
	w.fail = errors.New("local storage failed")
	err := RunWithNetwork(context.Background(), o, nil, w, Surface{}, r, NetworkOptions{n, func(context.Context) error { return nil }})
	if !errors.Is(err, w.fail) {
		t.Fatal(err)
	}
	await(t, o.stopped)
	await(t, w.stopped)
	await(t, r.stopped)
	await(t, n.stopped)
}
