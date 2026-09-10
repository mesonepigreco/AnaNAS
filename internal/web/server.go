// Package web serves local status and optional durable pause/resume controls.
//
// It binds only to loopback, embeds all assets, and never initiates NAS work.
// The caller owns the status provider and decides when the server is enabled.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot returns a JSON-marshalable read-only status value.
type Snapshot func() any

// PauseControl changes only the user's persistent pause intent. The transfer
// engine must independently enforce capability, LAN and per-path conflict gates.
type PauseControl func(bool) error

// Server is a loopback-only status server. New binds the listener so a port
// conflict is reported before the observer starts.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	errCh      chan error
	closeOnce  sync.Once
	updates    *updates
}

// New creates a server on 127.0.0.1. Port zero is useful for tests; production
// callers should use configuration port zero to disable the UI entirely.
func New(port int, snapshot Snapshot) (*Server, error) {
	return NewWithControl(port, snapshot, nil)
}

func NewWithControl(port int, snapshot Snapshot, pause PauseControl) (*Server, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("web port out of range")
	}
	if snapshot == nil {
		return nil, fmt.Errorf("web snapshot provider is required")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(secret[:])
	changes := newUpdates()
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler(token, pause != nil))
	mux.HandleFunc("/api/status", statusHandler(snapshot))
	mux.HandleFunc("/icon.svg", func(w http.ResponseWriter, r *http.Request) {
		if !methodAllowed(w, r) {
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(brandIcon)
	})
	mux.HandleFunc("/api/events", changes.handler(token, snapshot))
	if pause != nil {
		mux.HandleFunc("/api/pause", pauseHandler(token, func(p bool) error {
			if err := pause(p); err != nil {
				return err
			}
			changes.notify()
			return nil
		}))
	}
	server := &Server{
		httpServer: &http.Server{
			Handler:           hostGuard(mux),
			ReadHeaderTimeout: 2 * time.Second,
			ReadTimeout:       5 * time.Second,
			WriteTimeout:      5 * time.Second,
			IdleTimeout:       10 * time.Second,
			MaxHeaderBytes:    8 << 10,
		},
		listener: listener,
		updates:  changes,
		errCh:    make(chan error, 1),
	}
	return server, nil
}

// URL returns the loopback URL assigned to the listener.
func (s *Server) URL() string { return "http://" + s.listener.Addr().String() }

// Start begins serving. Errors other than an intentional shutdown are exposed
// through Errors and should cause the owning observer to stop.
func (s *Server) Start() {
	go func() {
		if err := s.httpServer.Serve(s.listener); err != nil && err != http.ErrServerClosed {
			select {
			case s.errCh <- err:
			default:
			}
		}
	}()
}

// Errors reports unexpected serving failures.
func (s *Server) Errors() <-chan error { return s.errCh }

// Notify coalesces a local status change without blocking the observer.
func (s *Server) Notify() { s.updates.notify() }

// Shutdown stops accepting requests and waits for active handlers to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.closeOnce.Do(func() { close(s.updates.done); err = s.httpServer.Shutdown(ctx) })
	return err
}

func hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !allowedHost(r.Host) {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !allowedOrigin(origin) {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func pauseHandler(token string, pause PauseControl) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Nas-Sync-CSRF")), []byte(token)) != 1 {
			http.Error(w, "invalid control token", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "controls require the same origin", http.StatusForbidden)
			return
		}
		kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || kind != "application/json" {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var request struct {
			Paused *bool `json:"paused"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&request); err != nil || request.Paused == nil {
			http.Error(w, "expected a paused boolean", http.StatusBadRequest)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "expected one JSON object", http.StatusBadRequest)
			return
		}
		if err := pause(*request.Paused); err != nil {
			http.Error(w, "could not persist pause state", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func allowedHost(host string) bool {
	if host == "" {
		return false
	}
	name := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		name = parsed
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		name = strings.Trim(host, "[]")
	}
	return name == "127.0.0.1" || name == "localhost" || name == "::1"
}

func allowedOrigin(raw string) bool {
	origin, err := url.Parse(raw)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil {
		return false
	}
	return allowedHost(origin.Host)
}

func methodAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func statusHandler(snapshot Snapshot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !methodAllowed(w, r) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(snapshot()); err != nil {
			return
		}
	}
}

//go:embed assets/ananas.svg
var brandIcon []byte

var pageTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>anaNAS status</title>
  <link rel="icon" href="/icon.svg" type="image/svg+xml">
  <meta name="nas-sync-csrf" content="{{.Token}}">
  <style>
    :root { color-scheme: light dark; font-family: system-ui, sans-serif; }
    body { max-width: 56rem; margin: 2rem auto; padding: 0 1rem; }
    pre { white-space: pre-wrap; overflow-wrap: anywhere; padding: 1rem; border: 1px solid #888; border-radius: .4rem; }
    .muted { opacity: .7; }
    button { padding: .6rem 1rem; cursor: pointer; }
    #message { min-height: 1.5rem; }
    h1 { display: flex; align-items: center; gap: .5rem; }
  </style>
</head>
<body>
  <h1><img src="/icon.svg" alt="" width="40" height="40">anaNAS</h1>
  <p id="mode">Loading daemon status…</p>
  {{if .Controls}}<button id="pause" disabled>Pause sync</button>{{end}}
  <p id="message" role="status"></p>
  <pre id="status">Loading…</pre>
  <script nonce="{{.Token}}">
    const output = document.getElementById('status');
    const pauseButton = document.getElementById('pause');
    const message = document.getElementById('message');
    let paused = false;
    let available = false;
    let controlling = false;
    let streamController;
    let reconnectTimer;
    let reconnectDelay = 1000;
    function setText(element, text) {
      if (element.textContent !== text) element.textContent = text;
    }
    function updateButton() {
      if (!pauseButton) return;
      pauseButton.disabled = controlling || !available;
      setText(pauseButton, paused ? 'Resume sync' : 'Pause sync');
    }
    async function controlToken(signal) {
      // A daemon restart rotates the token while an existing tab stays open.
      const response = await fetch('/', {cache: 'no-store', signal});
      if (!response.ok) throw new Error('Daemon controls unavailable');
      const page = new DOMParser().parseFromString(await response.text(), 'text/html');
      const token = page.querySelector('meta[name="nas-sync-csrf"]')?.content;
      if (!token || !/^[a-f0-9]{64}$/.test(token)) throw new Error('Daemon control token unavailable');
      return token;
    }
    function render(state) {
      if (!state || typeof state !== 'object') throw new Error('Invalid daemon status');
      setText(output, JSON.stringify(state, null, 2));
      setText(document.getElementById('mode'), state.syncReason || state.mode || 'Daemon status');
      paused = state.paused === true;
      available = true;
      reconnectDelay = 1000;
      updateButton();
    }
    async function connectStatus() {
      if (document.visibilityState !== 'visible' || streamController) return;
      clearTimeout(reconnectTimer);
      const controller = new AbortController();
      streamController = controller;
      try {
        const token = await controlToken(controller.signal);
        const response = await fetch('/api/events', {cache: 'no-store', signal: controller.signal,
          headers: {'X-Nas-Sync-CSRF': token}});
        if (!response.ok) throw new Error(response.status + ' ' + response.statusText);
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let pending = '';
        while (!controller.signal.aborted) {
          const chunk = await reader.read();
          if (chunk.done) throw new Error('Daemon status stream closed');
          pending += decoder.decode(chunk.value, {stream: true});
          if (pending.length > 65536) throw new Error('Daemon status exceeds size limit');
          let newline;
          while ((newline = pending.indexOf('\n')) !== -1) {
            const line = pending.slice(0, newline);
            pending = pending.slice(newline + 1);
            if (!controller.signal.aborted) render(JSON.parse(line));
          }
        }
      } catch (error) {
        if (!controller.signal.aborted) {
          available = false;
          setText(output, 'Status unavailable: ' + error);
          updateButton();
        }
      } finally {
        if (streamController === controller) streamController = undefined;
        if (!controller.signal.aborted && document.visibilityState === 'visible') {
          reconnectTimer = setTimeout(connectStatus, reconnectDelay);
          reconnectDelay = Math.min(reconnectDelay * 2, 30000);
        }
        controller.abort();
      }
    }
    if (pauseButton) pauseButton.addEventListener('click', async () => {
      if (controlling || !available) return;
      controlling = true;
      const requestedPause = !paused;
      pauseButton.disabled = true;
      try {
        const token = await controlToken();
        const response = await fetch('/api/pause', {method: 'POST', headers: {
          'Content-Type': 'application/json',
          'X-Nas-Sync-CSRF': token
        }, body: JSON.stringify({paused: requestedPause})});
        if (!response.ok) throw new Error(await response.text());
        message.textContent = requestedPause ? 'Sync paused. Local changes continue to be tracked.' : 'Resume requested. Transfers still require the NAS and LAN safety checks.';
      } catch (error) { message.textContent = 'Control failed: ' + error; }
      controlling = false;
      updateButton();
    });
    document.addEventListener('visibilitychange', () => {
      clearTimeout(reconnectTimer);
      if (document.visibilityState !== 'visible') {
        streamController?.abort();
        streamController = undefined;
        available = false;
        updateButton();
      } else {
        connectStatus();
      }
    });
    connectStatus();
  </script>
</body>
</html>`))

func indexHandler(token string, controls bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if !methodAllowed(w, r) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+token+"'; style-src 'unsafe-inline'; img-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		if err := pageTemplate.Execute(w, struct {
			Token    string
			Controls bool
		}{token, controls}); err != nil {
			return
		}
	}
}
