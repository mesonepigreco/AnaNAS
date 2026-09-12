package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Network interface {
	Watch(context.Context, chan<- struct{}) error
}

// Check performs local-only LAN validation, without DNS, NAS probes or root
// traversal. Per-operation gates are still mandatory.
type NetworkOptions struct {
	Monitor Network
	Check   func(context.Context) error
}

type NetworkStatus struct {
	Phase     string
	LastError string
}

// SessionRemote must cancel active I/O and discard pooled connections on
// Interrupt, without permanently closing the reusable client.
type SessionRemote interface {
	Remote
	Interrupt()
}

// RunWithNetwork preserves observation and controls across transfer sessions.
// Attempts subscribe before checking policy. Denied policy waits for a network
// event without a timer. Failed sessions retry at 1, 2, 4, ... 60 seconds; a
// session lasting at least a minute resets the delay. Healthy idle has no timer.
func RunWithNetwork(ctx context.Context, observer Observer, controls <-chan struct{}, worker Worker, surface Surface, remote SessionRemote, options NetworkOptions) error {
	if observer == nil || worker == nil || options.Monitor == nil || options.Check == nil {
		return fmt.Errorf("observer, sync worker, network monitor and local LAN check required")
	}
	w := &networkWorker{Worker: worker, remote: remote, options: options, report: surface.Network,
		minDelay: time.Second, maxDelay: time.Minute}
	return Run(ctx, observer, controls, w, surface)
}

// Forward bounded hints/controls while offline. The worker retains durable work
// and drains it on every Run; no observer or database is reopened for reconnect.
type networkWorker struct {
	Worker
	remote             SessionRemote
	options            NetworkOptions
	report             func(NetworkStatus)
	minDelay, maxDelay time.Duration
}

func (w *networkWorker) status(phase string, err error) {
	if w.report != nil {
		s := NetworkStatus{Phase: phase}
		if err != nil {
			s.LastError = err.Error()
		}
		w.report(s)
	}
}

func (w *networkWorker) Run(ctx context.Context) error {
	defer w.status("stopped", nil)
	delay := w.minDelay
	for ctx.Err() == nil {
		started := time.Now()
		err, fatal := w.attempt(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if fatal {
			return err
		}
		if time.Since(started) >= w.maxDelay {
			delay = w.minDelay
		}
		w.status("retrying", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		delay = min(2*delay, w.maxDelay)
	}
	return nil
}

func (w *networkWorker) attempt(ctx context.Context) (error, bool) {
	session, cancel := context.WithCancel(ctx)
	ready := make(chan struct{}, 1)
	networkDone := make(chan error, 1)
	var workerDone, remoteDone chan error
	defer func() {
		cancel()
		if w.remote != nil {
			w.remote.Interrupt()
		}
		// Reuse no worker/client/monitor until prior calls release resources.
		if workerDone != nil {
			<-workerDone
		}
		if remoteDone != nil {
			<-remoteDone
		}
		if networkDone != nil {
			<-networkDone
		}
		// An operation racing the first interruption may have returned a socket
		// to the pool. No old-session connection survives into the next attempt.
		if w.remote != nil {
			w.remote.Interrupt()
		}
	}()
	w.status("checking", nil)
	go func() {
		err := w.options.Monitor.Watch(session, ready)
		cancel() // interrupts the policy check and quiet stream immediately
		networkDone <- err
	}()
	// Channel variables change only after their goroutine sends its result.
	networkResult := func() (error, bool) {
		err := <-networkDone
		networkDone = nil
		if err == nil {
			err = fmt.Errorf("network monitor stopped unexpectedly")
		}
		return fmt.Errorf("network monitoring: %w", err), false
	}
	sessionResult := func() (error, bool) {
		var remoteErr error
		if remoteDone != nil {
			remoteErr = <-remoteDone
			remoteDone = nil
		}
		networkErr, _ := networkResult()
		if remoteErr != nil && !errors.Is(remoteErr, context.Canceled) {
			return fmt.Errorf("remote notifications: %w", remoteErr), false
		}
		return networkErr, false
	}
	select {
	case <-session.Done():
		return networkResult()
	case <-ready:
	}
	if session.Err() != nil {
		return networkResult()
	}
	if err := w.options.Check(session); err != nil {
		w.status("offline", err)
		<-session.Done() // no retry/probe timer while local policy denies access
		return networkResult()
	}
	if session.Err() != nil {
		return networkResult()
	}
	w.status("connecting", nil)
	workerDone = make(chan error, 1)
	go func() { workerDone <- w.Worker.Run(session) }()
	var hints chan struct{}
	if w.remote != nil {
		hints = make(chan struct{}, 1)
		remoteDone = make(chan error, 1)
		go func() {
			err := w.remote.WatchChanges(session, hints)
			cancel()
			remoteDone <- err
		}()
	} else {
		w.status("active", nil)
	}
	announced := false
	for {
		select {
		case <-session.Done():
			return sessionResult()
		case err := <-workerDone:
			workerDone = nil
			if session.Err() != nil {
				return sessionResult()
			}
			if err == nil {
				err = fmt.Errorf("sync worker stopped unexpectedly")
			}
			return err, true
		case _, ok := <-hints:
			if !ok {
				return fmt.Errorf("remote notification channel closed"), false
			}
			if session.Err() == nil {
				w.Worker.Notify()
				if !announced {
					w.status("active", nil)
					announced = true
				}
			}
		}
	}
}
