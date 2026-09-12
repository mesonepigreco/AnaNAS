package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventStreamsAreBoundedCoalescedAndCloseOnShutdown(t *testing.T) {
	var revision atomic.Uint64
	s, err := NewWithControl(0, func() any { return map[string]uint64{"revision": revision.Load()} }, func(bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Shutdown(context.Background())
	client := &http.Client{Timeout: 5 * time.Second}
	page, err := client.Get(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	match := regexp.MustCompile(`<meta name="nas-sync-csrf" content="([a-f0-9]{64})">`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("missing token")
	}
	open := func(token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", s.URL()+"/api/events", nil)
		req.Header.Set("X-Nas-Sync-CSRF", token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	bad := open("")
	bad.Body.Close()
	if bad.StatusCode != 403 {
		t.Fatal("stream lacks token guard")
	}
	streams := make([]*http.Response, 4)
	readers := make([]*bufio.Reader, 4)
	for i := range streams {
		streams[i] = open(string(match[1]))
		defer streams[i].Body.Close()
		if streams[i].StatusCode != 200 {
			t.Fatal(streams[i].Status)
		}
		readers[i] = bufio.NewReader(streams[i].Body)
		line, err := readers[i].ReadString('\n')
		if err != nil || !strings.Contains(line, `"revision":0`) {
			t.Fatalf("initial snapshot %s %v", line, err)
		}
	}
	extra := open(string(match[1]))
	extra.Body.Close()
	if extra.StatusCode != 503 {
		t.Fatal("unbounded subscribers")
	}
	for i := uint64(1); i <= 10000; i++ {
		revision.Store(i)
		s.Notify()
	}
	for _, reader := range readers {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]uint64
		if json.Unmarshal(line, &data) != nil || data["revision"] != 10000 {
			t.Fatalf("stale snapshot: %s", line)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, reader := range readers {
		if _, err := reader.ReadByte(); err != io.EOF {
			t.Fatalf("stream still open: %v", err)
		}
	}
}

func TestQuietEventStreamOutlivesOrdinaryWriteTimeout(t *testing.T) {
	s, err := New(0, func() any { return map[string]bool{"ready": true} })
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Shutdown(context.Background())
	client := &http.Client{Timeout: 10 * time.Second}
	page, err := client.Get(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	match := regexp.MustCompile(`<meta name="nas-sync-csrf" content="([a-f0-9]{64})">`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("missing token")
	}
	req, _ := http.NewRequest("GET", s.URL()+"/api/events", nil)
	req.Header.Set("X-Nas-Sync-CSRF", string(match[1]))
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// The server's ordinary write timeout is five seconds. An idle subscription
	// must survive beyond it without reconnects or heartbeat work.
	time.Sleep(6 * time.Second)
	s.Notify()
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"ready":true`) {
		t.Fatalf("idle stream: %q %v", line, err)
	}
}
