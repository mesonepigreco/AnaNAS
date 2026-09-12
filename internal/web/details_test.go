package web

import (
	"context"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestStorageRequiresTokenAndPassesCancellationAndPath(t *testing.T) {
	calls := 0
	s, e := NewWithDetails(0, func() any { return nil }, nil, func(ctx context.Context, path string) (any, error) {
		calls++
		if path != "SampleDocuments/sub folder" {
			t.Fatal(path)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline")
		}
		return map[string]int{"bytes": 42}, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	defer s.listener.Close()
	page := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(page, httptest.NewRequest("GET", s.URL()+"/", nil))
	token := regexp.MustCompile(`name="nas-sync-csrf" content="([a-f0-9]{64})"`).FindStringSubmatch(page.Body.String())[1]
	for _, valid := range []bool{false, true} {
		r := httptest.NewRequest("GET", s.URL()+"/api/storage?path=SampleDocuments%2Fsub+folder", nil)
		if valid {
			r.Header.Set("X-Nas-Sync-CSRF", token)
		}
		w := httptest.NewRecorder()
		s.httpServer.Handler.ServeHTTP(w, r)
		want := 403
		if valid {
			want = 200
		}
		if w.Code != want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
