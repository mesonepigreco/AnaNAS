package main

import (
	"testing"

	"nas-sync/internal/daemon"
	"nas-sync/internal/observe"
	"nas-sync/internal/replica"
)

func TestStatusReasonDistinguishesObservationProgressFromFailures(t *testing.T) {
	active := daemon.NetworkStatus{Phase: "active"}
	tests := []struct {
		name        string
		network     daemon.NetworkStatus
		observation observe.Status
		worker      replica.WorkerStatus
		want        string
	}{
		{"network failure takes precedence", daemon.NetworkStatus{Phase: "offline", LastError: "no direct LAN"}, observe.Status{Ready: true, Scanning: true}, replica.WorkerStatus{}, "offline: no direct LAN"},
		{"network transition", daemon.NetworkStatus{Phase: "connecting"}, observe.Status{Ready: true}, replica.WorkerStatus{}, "LAN sync: connecting"},
		{"observer failure", active, observe.Status{LastError: "watch limit reached"}, replica.WorkerStatus{}, "Local observation error: watch limit reached"},
		{"initial index", active, observe.Status{}, replica.WorkerStatus{}, "Preparing local file index; synchronization will start automatically"},
		{"incremental index", active, observe.Status{Ready: true, Scanning: true}, replica.WorkerStatus{Phase: "suspended", LastError: "synchronization suspended: local observation is not ready"}, "Indexing local changes; synchronization will resume automatically"},
		{"worker failure", active, observe.Status{Ready: true}, replica.WorkerStatus{Phase: "attention", LastError: "publication cache budget exhausted"}, "Synchronization needs attention: publication cache budget exhausted"},
		{"worker retry", active, observe.Status{Ready: true}, replica.WorkerStatus{Phase: "retrying", LastError: "connection reset"}, "Temporary synchronization problem; retrying automatically: connection reset"},
		{"working", active, observe.Status{Ready: true}, replica.WorkerStatus{Phase: "working"}, "Synchronizing"},
		{"idle", active, observe.Status{Ready: true}, replica.WorkerStatus{Phase: "idle"}, "LAN sync active"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusReason(tt.network, tt.observation, tt.worker); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
