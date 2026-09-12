// Package daemon connects local observation, durable controls and optional sync
// work. It owns service lifetimes and forwards bounded hints, never file content.
package daemon

import (
	"context"
	"fmt"
)

type Observer interface {
	Run(context.Context) error
	Changes() <-chan struct{}
	Reconciled() <-chan struct{}
}

type Worker interface {
	Run(context.Context) error
	Notify()
	Interrupt()
	Changes() <-chan struct{}
}

type Surface struct {
	Notify func()
	Errors <-chan error
	// Network receives session transitions. Like Notify it must not block;
	// the caller owns display storage and synchronization. Active is not synced.
	Network func(NetworkStatus)
}

type Remote interface {
	WatchChanges(context.Context, chan<- struct{}) error
}

// Run owns one observer and, when provided, one transfer worker. It forwards
// completed metadata batches separately from display updates. No timer or queue
// of paths is added. Every started goroutine is joined before returning, so the
// caller can safely close the index and pinned files afterward. A nil worker
// preserves local-only operation and cannot initiate NAS traffic.
func Run(ctx context.Context, observer Observer, controls <-chan struct{}, worker Worker, surface Surface) error {
	return RunWithRemote(ctx, observer, controls, worker, surface, nil)
}

// RunWithRemote additionally owns one authenticated remote hint stream. A stream
// failure stops and joins the lifecycle, leaving durable cursors for a supervised
// restart. It does not silently claim freshness after losing notifications and
// does not retry from an internal timer. Content pause still permits metadata
// hints; the worker checks durable pause intent before acting on every hint.
func RunWithRemote(ctx context.Context, observer Observer, controls <-chan struct{}, worker Worker, surface Surface, remote Remote) error {
	if observer == nil {
		return fmt.Errorf("observer required")
	}
	if remote != nil && worker == nil {
		return fmt.Errorf("remote notifications require a sync worker")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	observerDone := make(chan error, 1)
	go func() { observerDone <- observer.Run(runCtx) }()
	var workerDone chan error
	var workChanges <-chan struct{}
	if worker != nil {
		workerDone = make(chan error, 1)
		workChanges = worker.Changes()
		go func() { workerDone <- worker.Run(runCtx) }()
	}
	var remoteDone chan error
	var remoteHints chan struct{}
	if remote != nil {
		remoteDone = make(chan error, 1)
		remoteHints = make(chan struct{}, 1)
		go func() { remoteDone <- remote.WatchChanges(runCtx, remoteHints) }()
	}
	observationChanges, reconciled := observer.Changes(), observer.Reconciled()
	uiErrors := surface.Errors
	notify := func() {
		if surface.Notify != nil {
			surface.Notify()
		}
	}
	var result error
	running := true
	for running {
		select {
		case <-ctx.Done():
			running = false
		case result = <-observerDone:
			observerDone = nil
			running = false
		case result = <-workerDone:
			workerDone = nil
			if result == nil && ctx.Err() == nil {
				result = fmt.Errorf("sync worker stopped unexpectedly")
			}
			running = false
		case err := <-remoteDone:
			remoteDone = nil
			if err == nil {
				err = fmt.Errorf("stream ended")
			}
			result = fmt.Errorf("remote notifications: %w", err)
			running = false
		case _, ok := <-remoteHints:
			if !ok {
				result = fmt.Errorf("remote notification channel closed")
				running = false
				continue
			}
			worker.Notify()
		case err, ok := <-uiErrors:
			if !ok {
				uiErrors = nil
				continue
			}
			if err != nil {
				result = fmt.Errorf("local status UI: %w", err)
				running = false
			}
		case _, ok := <-observationChanges:
			if !ok {
				observationChanges = nil
				continue
			}
			notify()
		case _, ok := <-reconciled:
			if !ok {
				reconciled = nil
				continue
			}
			if worker != nil {
				worker.Notify()
			}
		case _, ok := <-controls:
			if !ok {
				controls = nil
				continue
			}
			if worker != nil {
				worker.Interrupt()
			}
			notify()
		case _, ok := <-workChanges:
			if !ok {
				workChanges = nil
				continue
			}
			notify()
		}
	}
	cancel()
	if observerDone != nil {
		if err := <-observerDone; result == nil {
			result = err
		}
	}
	if workerDone != nil {
		if err := <-workerDone; result == nil {
			result = err
		}
	}
	if remoteDone != nil {
		<-remoteDone // shared cancellation is expected; preserve the primary error
	}
	if ctx.Err() != nil {
		return nil
	}
	return result
}
