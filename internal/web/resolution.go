package web

import (
	"encoding/json"
	"io"
	"nas-sync/internal/index"
	"net/http"
)

func recoveryRequest(w http.ResponseWriter, r *http.Request, token string, value any) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", 405)
		return false
	}
	if !controlToken(w, r, token) {
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "Invalid recovery request", 400)
		return false
	}
	return true
}
func resolutionHandler(token string, control TreeControls) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request index.IncomingChoice
		if !recoveryRequest(w, r, token, &request) {
			return
		}
		if err := control.Resolve(r.Context(), request); err != nil {
			http.Error(w, "This problem changed or recovery is already queued. Refresh and review again.", 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"queued": true})
	}
}
func notificationHandler(token string, control TreeControls) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Enabled *bool `json:"enabled"`
		}
		if !recoveryRequest(w, r, token, &request) {
			return
		}
		if request.Enabled == nil || control.Notifications == nil {
			http.Error(w, "Notification setting required", 400)
			return
		}
		if err := control.Notifications(r.Context(), *request.Enabled); err != nil {
			http.Error(w, "Could not save notification preference", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"saved": true})
	}
}
