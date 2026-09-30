package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTreeAndDirectoryPriorityRequireControlToken(t *testing.T) {
	calls := 0
	controls := TreeControls{Snapshot: func(ctx context.Context, path, after string) (any, error) {
		if path != "folder/child" || after != "next" {
			t.Fatal(path, after)
		}
		calls++
		return map[string]bool{"ok": true}, nil
	}, Prioritize: func(ctx context.Context, path string) error {
		calls++
		if path != "folder/child" {
			t.Fatal(path)
		}
		return nil
	}}
	for _, token := range []string{"", "token"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1/api/nas-tree?path=folder%2Fchild&after=next", nil)
		r.Header.Set("X-Nas-Sync-CSRF", token)
		w := httptest.NewRecorder()
		treeHandler("token", controls)(w, r)
		if token == "" && w.Code != 403 || token != "" && w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	for _, tc := range []struct {
		body, token string
		code        int
	}{{`{"path":"folder/child"}`, "", 403}, {`{}`, "token", 400}, {`{"path":"folder/child","extra":true}`, "token", 400}, {`{"path":"folder/child"} {}`, "token", 400}, {`{"path":"folder/child"}`, "token", 200}} {
		r := httptest.NewRequest("POST", "http://127.0.0.1/api/prioritize-directory", strings.NewReader(tc.body))
		r.Header.Set("X-Nas-Sync-CSRF", tc.token)
		w := httptest.NewRecorder()
		priorityHandler("token", controls)(w, r)
		if w.Code != tc.code {
			t.Fatal(tc, w.Code)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
