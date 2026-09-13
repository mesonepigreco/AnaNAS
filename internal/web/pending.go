package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"nas-sync/internal/index"
)

type ConfirmControl func(context.Context, index.SyncConfirmation) (index.ConfirmationResult, error)

func controlToken(w http.ResponseWriter, r *http.Request, token string) bool {
	w.Header().Set("Cache-Control", "no-store")
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Nas-Sync-CSRF")), []byte(token)) != 1 || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+r.Host) {
		http.Error(w, "invalid control token or origin", http.StatusForbidden)
		return false
	}
	return true
}

func pendingHandler(token string, pending Details) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !methodAllowed(w, r) || !controlToken(w, r, token) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		value, err := pending(ctx, r.URL.Query().Get("after"))
		if err != nil {
			http.Error(w, "Pending files could not be loaded", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(value)
	}
}

func confirmHandler(token string, confirm ConfirmControl) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if !controlToken(w, r, token) {
			return
		}
		kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || kind != "application/json" {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var request index.SyncConfirmation
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "Invalid confirmation", http.StatusBadRequest)
			return
		}
		if decoder.Decode(new(any)) != io.EOF || (request.All && (request.Path != "" || request.Generation != 0)) || (!request.All && (request.Path == "" || request.Generation == 0)) {
			http.Error(w, "Choose all or a path and generation", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		result, err := confirm(ctx, request)
		if err != nil {
			if errors.Is(err, index.ErrStale) {
				http.Error(w, "File changed; refresh and confirm again", http.StatusConflict)
			} else {
				http.Error(w, "Could not queue synchronization", http.StatusServiceUnavailable)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}
