package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func startTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(0, func() any {
		return map[string]any{"ready": true, "events": 3}
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return server
}

func TestPauseControlsRequireTokenSameOriginAndBoundedJSON(t *testing.T) {
	paused := false
	fail := false
	calls := 0
	server, err := NewWithControl(0, func() any { return map[string]bool{"paused": paused} }, func(p bool) error {
		calls++
		if fail {
			return errors.New("disk full")
		}
		paused = p
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the same middleware/handlers synchronously; status remains bound
	// to loopback, while control state tests need no concurrent shared variables.
	defer server.listener.Close()
	page := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(page, httptest.NewRequest("GET", server.URL()+"/", nil))
	match := regexp.MustCompile(`<meta name="nas-sync-csrf" content="([a-f0-9]{64})">`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal("control token missing from embedded page")
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing page protections")
	}
	token := match[1]
	cases := []struct {
		name, method, body, token, origin, kind string
		status                                  int
	}{
		{"no token", "POST", `{"paused":true}`, "", server.URL(), "application/json", 403},
		{"wrong token", "POST", `{"paused":true}`, strings.Repeat("0", 64), server.URL(), "application/json", 403},
		{"foreign origin", "POST", `{"paused":true}`, token, "https://evil.example", "application/json", 403},
		{"other local app", "POST", `{"paused":true}`, token, "http://127.0.0.1:1", "application/json", 403},
		{"GET cannot mutate", "GET", "", token, server.URL(), "application/json", 405},
		{"form cannot mutate", "POST", `paused=true`, token, server.URL(), "application/x-www-form-urlencoded", 415},
		{"null", "POST", `{"paused":null}`, token, server.URL(), "application/json", 400},
		{"missing", "POST", `{}`, token, server.URL(), "application/json", 400},
		{"unknown setting", "POST", `{"paused":true,"automaticWrites":true}`, token, server.URL(), "application/json", 400},
		{"multiple objects", "POST", `{"paused":true}{"paused":false}`, token, server.URL(), "application/json", 400},
		{"oversized", "POST", strings.Repeat(" ", 1024) + `{"paused":true}`, token, server.URL(), "application/json", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, server.URL()+"/api/pause", strings.NewReader(tc.body))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Content-Type", tc.kind)
			r.Header.Set("X-Nas-Sync-CSRF", tc.token)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatal("rejected control reached state mutation")
	}
	for _, p := range []bool{true, false} {
		r := httptest.NewRequest("POST", server.URL()+"/api/pause", strings.NewReader(fmt.Sprintf(`{"paused":%t}`, p)))
		r.Header.Set("Origin", server.URL())
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Nas-Sync-CSRF", token)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, r)
		if w.Code != 204 || paused != p {
			t.Fatalf("valid control failed: %d %s", w.Code, w.Body.String())
		}
	}
	fail = true
	r := httptest.NewRequest("POST", server.URL()+"/api/pause", strings.NewReader(`{"paused":true}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Nas-Sync-CSRF", token)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, r)
	if w.Code != 500 || paused {
		t.Fatal("failed persistence was reported as successful")
	}
}

func TestServerServesEmbeddedPageAndStatus(t *testing.T) {
	server := startTestServer(t)
	client := &http.Client{Timeout: time.Second}

	page, err := client.Get(server.URL() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	pageBody, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "anaNAS status") {
		t.Fatalf("page status=%d body=%q", page.StatusCode, pageBody)
	}

	response, err := client.Get(server.URL() + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status map[string]any
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || status["ready"] != true || status["events"] != float64(3) {
		t.Fatalf("status=%d payload=%v", response.StatusCode, status)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control=%q", got)
	}
}

func TestServerRejectsUnsafeHostOriginAndMethods(t *testing.T) {
	server := startTestServer(t)
	client := &http.Client{Timeout: time.Second}

	request, err := http.NewRequest(http.MethodGet, server.URL()+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "evil.example"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsafe host status=%d", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL()+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "http://evil.example")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unsafe origin status=%d", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodPost, server.URL()+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("post status=%d", response.StatusCode)
	}
}
