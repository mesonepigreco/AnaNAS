package web

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// At most four local UI streams, one pending notification per stream. A slow
// reader cannot hold up observation. No heartbeat or timer runs while idle.
type updates struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
	done    chan struct{}
}

func newUpdates() *updates {
	return &updates{clients: make(map[chan struct{}]struct{}), done: make(chan struct{})}
}

func (u *updates) notify() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for ch := range u.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (u *updates) handler(token string, snapshot Snapshot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Nas-Sync-CSRF")), []byte(token)) != 1 {
			http.Error(w, "invalid control token", http.StatusForbidden)
			return
		}
		ch := make(chan struct{}, 1)
		u.mu.Lock()
		if len(u.clients) >= 4 {
			u.mu.Unlock()
			http.Error(w, "too many status streams", http.StatusServiceUnavailable)
			return
		}
		u.clients[ch] = struct{}{}
		u.mu.Unlock()
		defer func() { u.mu.Lock(); delete(u.clients, ch); u.mu.Unlock() }()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		rc := http.NewResponseController(w)
		write := func() bool {
			if rc.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
				return false
			}
			if json.NewEncoder(w).Encode(snapshot()) != nil || rc.Flush() != nil {
				return false
			}
			return rc.SetWriteDeadline(time.Time{}) == nil
		}
		if !write() {
			return
		}
		next := time.Now().Add(time.Second)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-u.done:
				return
			case <-ch:
			}
			if delay := time.Until(next); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-r.Context().Done():
					timer.Stop()
					return
				case <-u.done:
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			// Events arriving during the rate limit are covered by this snapshot.
			select {
			case <-ch:
			default:
			}
			if !write() {
				return
			}
			next = time.Now().Add(time.Second)
		}
	}
}
