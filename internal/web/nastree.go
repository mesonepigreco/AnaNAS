package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type TreeControls struct {
	Snapshot   func(context.Context, string, string) (any, error)
	Prioritize func(context.Context, string) error
}

func treeHandler(token string, tree TreeControls) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !methodAllowed(w, r) || !controlToken(w, r, token) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		result, err := tree.Snapshot(ctx, r.URL.Query().Get("path"), r.URL.Query().Get("after"))
		if err != nil {
			http.Error(w, "NAS inventory unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}
func priorityHandler(token string, tree TreeControls) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST required", 405)
			return
		}
		if !controlToken(w, r, token) {
			return
		}
		var request struct {
			Path *string `json:"path"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || request.Path == nil || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "directory path required", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		if err := tree.Prioritize(ctx, *request.Path); err != nil {
			http.Error(w, "Could not prioritize directory; refresh and retry", 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"queued": true})
	}
}
