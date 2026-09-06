package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "nas-sync status") {
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
