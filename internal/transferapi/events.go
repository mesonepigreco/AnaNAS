package transferapi

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const eventFrameLimit = 256
const eventInterval = time.Second

type changeEvent struct {
	Namespace string `json:"namespace"`
	Epoch     uint64 `json:"epoch"`
	Through   uint64 `json:"through"`
}

type eventStream struct {
	cancel context.CancelFunc
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Connection", "close")
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawQuery != "" {
		fail(w, 400, fmt.Errorf("notification request must have no body or query"))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream := &eventStream{cancel: cancel}
	s.streamsMu.Lock()
	previous := s.streams[id]
	if previous == nil && len(s.streams) >= s.opts.MaxEventStreams {
		s.streamsMu.Unlock()
		fail(w, 503, fmt.Errorf("notification stream limit reached"))
		return
	}
	// A reboot or silent link loss can leave an idle TCP stream alive. The
	// authenticated identity may replace its own stream without another slot.
	if previous != nil {
		previous.cancel()
	}
	s.streams[id] = stream
	s.streamsMu.Unlock()
	defer func() {
		s.streamsMu.Lock()
		if s.streams[id] == stream {
			delete(s.streams, id)
		}
		s.streamsMu.Unlock()
	}()
	gate := func() error {
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return s.opts.Gate(check)
	}
	if err := gate(); err != nil {
		fail(w, 503, err)
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		fail(w, 500, err)
		return
	}
	defer controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/x-ananas-events")
	var last uint64
	var sent time.Time
	for {
		sequence, changed, err := s.c.Observe()
		if err != nil || ctx.Err() != nil {
			return
		}
		if sent.IsZero() || sequence != last {
			// Only actual changes arm this one-shot coalescing timer. Idle streams
			// have no heartbeat, deadline or repeated journal read.
			if wait := time.Until(sent.Add(eventInterval)); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				sequence, changed, err = s.c.Observe()
				if err != nil {
					return
				}
			}
			if err := gate(); err != nil {
				return
			}
			if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			if err := WriteFrame(w, changeEvent{s.c.Namespace(), s.c.Epoch(), sequence}, eventFrameLimit); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			if err := controller.SetWriteDeadline(time.Time{}); err != nil {
				return
			}
			last, sent = sequence, time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// WatchChanges occupies one separate connection alongside the single transfer
// operation. Every connection begins with a current high-water hint, so reconnect
// cannot lose changes; the durable inbox cursor, never this hint, governs replay.
// The caller supplies a one-slot channel and owns cancellation/reconnect policy.
// There is no automatic retry, heartbeat or idle read timer. A silent broken link
// therefore needs a network lifecycle signal to cancel it.
func (c *Client) WatchChanges(ctx context.Context, hints chan<- struct{}) error {
	if hints == nil || cap(hints) != 1 {
		return fmt.Errorf("one-slot notification channel required")
	}
	c.mu.Lock()
	if c.closed || c.watching {
		c.mu.Unlock()
		return fmt.Errorf("client closed or notification stream already active")
	}
	ctx, cancel := context.WithCancel(ctx)
	c.watching, c.watchCancel = true, cancel
	c.mu.Unlock()
	defer func() { cancel(); c.mu.Lock(); c.watching, c.watchCancel = false, nil; c.mu.Unlock() }()
	if err := c.opts.Gate(ctx); err != nil {
		return err
	}
	r, err := c.request(ctx, http.MethodGet, "/v1/events", nil, 0)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/x-ananas-events" {
		return fmt.Errorf("unexpected notification content type")
	}
	reader := bufio.NewReaderSize(r.Body, 4096)
	var last changeEvent
	var delivered time.Time
	for {
		var event changeEvent
		if err := ReadFrame(reader, &event, eventFrameLimit); err != nil {
			return err
		}
		if event.Namespace != c.opts.Namespace || event.Epoch == 0 || (last.Epoch != 0 && (event.Epoch != last.Epoch || event.Through <= last.Through)) {
			return fmt.Errorf("invalid or regressed notification identity")
		}
		// Bound client work even if an authenticated peer ignores the server's
		// coalescing contract. This timer exists only after an actual frame arrives.
		if wait := time.Until(delivered.Add(eventInterval)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := c.opts.Gate(ctx); err != nil {
			return err
		}
		last = event
		delivered = time.Now()
		select {
		case hints <- struct{}{}:
		default:
		}
	}
}
