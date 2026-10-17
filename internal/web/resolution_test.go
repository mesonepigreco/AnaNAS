package web

import (
	"context"
	"nas-sync/internal/index"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoveryEndpointsRejectStaleOrUnauthenticatedChoices(t *testing.T) {
	for _, test := range []struct {
		body, token, origin string
		want                int
	}{
		{`{"path":"folder","revision":"version","choice":"nas"}`, "", "", 403},
		{`{"path":"folder","revision":"version","choice":"nas"}`, "secret", "https://untrusted.example", 403},
		{`{"path":"folder","revision":"version","choice":"nas","force":true}`, "secret", "", 400},
		{`{"path":"folder","revision":"stale","choice":"nas"}`, "secret", "", 409},
		{`{"path":"folder","revision":"version","choice":"nas"}`, "secret", "", 200},
	} {
		called := false
		control := TreeControls{Resolve: func(_ context.Context, r index.IncomingChoice) error {
			called = true
			if r.Revision == "stale" {
				return index.ErrStale
			}
			return nil
		}}
		r := httptest.NewRequest("POST", "http://127.0.0.1/api/resolve-sync", strings.NewReader(test.body))
		r.Header.Set("X-Nas-Sync-CSRF", test.token)
		r.Header.Set("Origin", test.origin)
		w := httptest.NewRecorder()
		resolutionHandler("secret", control)(w, r)
		if w.Code != test.want || (test.want == 400 || test.want == 403) && called {
			t.Fatal(test, w.Code, called)
		}
	}
}
