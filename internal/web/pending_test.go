package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"nas-sync/internal/index"
)

func TestConfirmationRequiresTokenOriginAndStrictRequest(t *testing.T) {
	calls := 0
	h := confirmHandler("token", func(ctx context.Context, r index.SyncConfirmation) (index.ConfirmationResult, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline")
		}
		return index.ConfirmationResult{Queued: 1}, nil
	})
	for _, tc := range []struct {
		body, token, origin string
		want                int
	}{
		{`{"all":true}`, "", "", 403},
		{`{"all":true}`, "token", "http://localhost:9999", 403},
		{`{}`, "token", "", 400},
		{`{"all":true,"path":"file"}`, "token", "", 400},
		{`{"all":true,"unknown":1}`, "token", "", 400},
		{`{"all":true} {}`, "token", "", 400},
		{`{"path":"file","generation":1}`, "token", "", 200},
		{`{"all":true}`, "token", "", 200},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8721/api/confirm-sync", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Nas-Sync-CSRF", tc.token)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != tc.want {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestPendingReadRequiresTokenAndPreservesCursor(t *testing.T) {
	calls := 0
	h := pendingHandler("token", func(ctx context.Context, after string) (any, error) {
		calls++
		if after != "folder/file" {
			t.Fatal(after)
		}
		return index.PendingFiles{}, nil
	})
	for _, token := range []string{"", "token"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1/api/pending?after=folder%2Ffile", nil)
		r.Header.Set("X-Nas-Sync-CSRF", token)
		w := httptest.NewRecorder()
		h(w, r)
		if token == "" && w.Code != 403 || token != "" && w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
