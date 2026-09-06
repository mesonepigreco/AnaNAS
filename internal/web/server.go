// Package web serves the local, read-only status surface.
//
// It binds only to loopback, embeds all assets, and never initiates NAS work.
// The caller owns the status provider and decides when the server is enabled.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
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

// Server is a loopback-only status server. New binds the listener so a port
// conflict is reported before the observer starts.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	errCh      chan error
	closeOnce  sync.Once
}

// New creates a server on 127.0.0.1. Port zero is useful for tests; production
// callers should use configuration port zero to disable the UI entirely.
func New(port int, snapshot Snapshot) (*Server, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("web port out of range")
	}
	if snapshot == nil {
		return nil, fmt.Errorf("web snapshot provider is required")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/api/status", statusHandler(snapshot))
	server := &Server{
		httpServer: &http.Server{
			Handler:           hostGuard(mux),
			ReadHeaderTimeout: 2 * time.Second,
			IdleTimeout:       10 * time.Second,
			MaxHeaderBytes:    8 << 10,
		},
		listener: listener,
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

// Shutdown stops accepting requests and waits for active handlers to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.closeOnce.Do(func() { err = s.httpServer.Shutdown(ctx) })
	return err
}

func hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

var pageTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>nas-sync status</title>
  <style>
    :root { color-scheme: light dark; font-family: system-ui, sans-serif; }
    body { max-width: 56rem; margin: 2rem auto; padding: 0 1rem; }
    pre { white-space: pre-wrap; overflow-wrap: anywhere; padding: 1rem; border: 1px solid #888; border-radius: .4rem; }
    .muted { opacity: .7; }
  </style>
</head>
<body>
  <h1>nas-sync</h1>
  <p class="muted">Local metadata status. NAS synchronization is controlled by the daemon policy.</p>
  <pre id="status">Loading…</pre>
  <script>
    const output = document.getElementById('status');
    let timer;
    async function refresh() {
      if (document.visibilityState !== 'visible') return;
      try {
        const response = await fetch('/api/status', {cache: 'no-store'});
        if (!response.ok) throw new Error(response.status + ' ' + response.statusText);
        output.textContent = JSON.stringify(await response.json(), null, 2);
      } catch (error) {
        output.textContent = 'Status unavailable: ' + error;
      }
      clearTimeout(timer);
      if (document.visibilityState === 'visible') timer = setTimeout(refresh, 5000);
    }
    document.addEventListener('visibilitychange', refresh);
    refresh();
  </script>
</body>
</html>`))

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, nil); err != nil {
		return
	}
}
