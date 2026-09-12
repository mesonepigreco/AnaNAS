package transferapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"nas-sync/internal/journal"
)

func eventAwait(t *testing.T, hints <-chan struct{}) {
	t.Helper()
	select {
	case <-hints:
	case <-time.After(5 * time.Second):
		t.Fatal("remote hint missing")
	}
}

func eventJoined(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("stream failed to stop")
		return nil
	}
}

func TestTLSNotificationsCoalesceRemainIdleAndReconnect(t *testing.T) {
	f := setup(t, true, nil)
	c := newTestClient(t, clientOptions(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hints, done := make(chan struct{}, 1), make(chan error, 1)
	go func() { done <- c.WatchChanges(ctx, hints) }()
	eventAwait(t, hints)
	if err := c.WatchChanges(ctx, hints); err == nil {
		t.Fatal("second client stream accepted")
	}
	duplicate := newTestClient(t, clientOptions(f))
	err := duplicate.WatchChanges(ctx, make(chan struct{}, 1))
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Status != http.StatusServiceUnavailable {
		t.Fatal("same identity opened another stream", err)
	}
	duplicate.Close()
	// A transfer on the same Client must work while its event connection waits.
	data := []byte("notification fixture")
	wire := encoded(t, nil, data)
	request, _ := f.request(t, "event-create", "file", "", data, wire)
	if _, err := c.Apply(ctx, request, []io.Reader{bytes.NewReader(wire)}); err != nil {
		t.Fatal(err)
	}
	eventAwait(t, hints)
	page, err := c.ChangesPage(ctx, "", "", 0, 32)
	if err != nil || page.Through != 1 {
		t.Fatal(page, err)
	}
	// A stopped reader leaves a single pending hint; subsequent transfers cannot
	// wait for that reader or accumulate one notification per changed path.
	for i := 0; i < 8; i++ {
		name := string(rune('a' + i))
		r, _ := f.request(t, "event-"+name, name, "", data, wire)
		if _, err := c.Apply(ctx, r, []io.Reader{bytes.NewReader(wire)}); err != nil {
			t.Fatal(err)
		}
	}
	eventAwait(t, hints)
	time.Sleep(1100 * time.Millisecond) // let a coalesced trailing commit drain
	select {
	case <-hints:
	default:
	}
	idle := c.Traffic()
	time.Sleep(1100 * time.Millisecond)
	if c.Traffic() != idle {
		t.Fatal("idle notification stream emitted traffic", idle, c.Traffic())
	}
	cancel()
	if err := eventJoined(t, done); err == nil {
		t.Fatal("unexpected successful stream EOF")
	}
	// Publish while disconnected, then explicitly reconnect. The initial hint
	// causes replay from the retained cursor and includes that missed commit.
	r, _ := f.request(t, "event-offline", "offline", "", data, wire)
	if _, err := c.Apply(context.Background(), r, []io.Reader{bytes.NewReader(wire)}); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { done <- c.WatchChanges(ctx2, hints) }()
	eventAwait(t, hints)
	page, err = c.ChangesPage(ctx2, page.Namespace, page.Policy, page.Through, 32)
	if err != nil || page.Through != 10 || len(page.Batches) != 9 {
		t.Fatal(page, err)
	}
	c.Close()
	if err := eventJoined(t, done); err == nil {
		t.Fatal("Close left notification stream open")
	}
}

func TestNotificationGateRefusalMakesNoConnection(t *testing.T) {
	f := setup(t, false, nil)
	opts := clientOptions(f)
	want := errors.New("LAN unavailable")
	opts.Gate = func(context.Context) error { return want }
	c := newTestClient(t, opts)
	if err := c.WatchChanges(context.Background(), make(chan struct{}, 1)); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if c.Traffic() != (Traffic{}) {
		t.Fatal("denied notification produced traffic")
	}
}

func TestNotificationRejectsMalformedAuthenticatedFrames(t *testing.T) {
	valid := changeEvent{Namespace: identifier("namespace"), Epoch: 1, Through: 4}
	for _, test := range []struct {
		name     string
		events   []changeEvent
		oversize bool
	}{
		{name: "wrong namespace", events: []changeEvent{{Namespace: identifier("other"), Epoch: 1}}},
		{name: "zero epoch", events: []changeEvent{{Namespace: valid.Namespace}}},
		{name: "regressed", events: []changeEvent{valid, {Namespace: valid.Namespace, Epoch: 1, Through: 3}}},
		{name: "repeated", events: []changeEvent{valid, valid}},
		{name: "epoch changed", events: []changeEvent{valid, {Namespace: valid.Namespace, Epoch: 2, Through: 5}}},
		{name: "oversized", oversize: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := clientWithPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ananas-events")
				if test.oversize {
					var header [4]byte
					binary.BigEndian.PutUint32(header[:], eventFrameLimit+1)
					w.Write(header[:])
				} else {
					for _, event := range test.events {
						WriteFrame(w, event, eventFrameLimit)
					}
				}
			}))
			err := c.WatchChanges(context.Background(), make(chan struct{}, 1))
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatal("invalid frame not rejected", err)
			}
		})
	}
}

func TestNotificationRechecksServerGateBeforeSendingChange(t *testing.T) {
	f := setup(t, false, nil)
	var denied atomic.Bool
	f.api.opts.Gate = func(context.Context) error {
		if denied.Load() {
			return errors.New("route no longer allowed")
		}
		return nil
	}
	c := newTestClient(t, clientOptions(f))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hints, done := make(chan struct{}, 1), make(chan error, 1)
	go func() { done <- c.WatchChanges(ctx, hints) }()
	eventAwait(t, hints)
	denied.Store(true)
	// Metadata-only direct journal fixture simulates a different source commit.
	// No content publication or real network route mutation is claimed here.
	p := journal.Proposal{ID: identifier("gate-op"), Client: f.clientID, Entries: []journal.Entry{{Path: "file", Next: journal.Version{ID: identifier("gate-version"), Tombstone: true}}}}
	if _, err := f.c.Commit(ctx, f.c.Epoch(), p, metadataPublisher{}); err != nil {
		t.Fatal(err)
	}
	if err := eventJoined(t, done); err == nil {
		t.Fatal("denied stream remained active")
	}
	select {
	case <-hints:
		t.Fatal("denied change emitted a hint")
	default:
	}
}
