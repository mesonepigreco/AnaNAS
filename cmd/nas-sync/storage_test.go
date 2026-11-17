package main

import (
	"context"
	"encoding/json"
	"nas-sync/internal/config"
	"nas-sync/internal/index"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorageDoesNotReportLocalDiskWhenNASUnmounted(t *testing.T) {
	nas := config.NAS{MountPoint: t.TempDir(), Host: "10.23.42.30", Share: "Nasdir", Protocol: "smb"}
	s := storageView{nas: func() config.NAS { return nas }}
	c := s.space(context.Background())
	if c.Available || c.Total != 0 || c.Message == "" {
		t.Fatalf("local disk reported as NAS: %+v", c)
	}
}

func TestStorageReturnsCapacityWhenUsageDeadlineExpires(t *testing.T) {
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"), "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Put([]index.Record{{Path: "file", Fingerprint: index.Fingerprint{Size: 42}}}, false); err != nil {
		t.Fatal(err)
	}
	cached := capacity{Available: true, Total: 100, Used: 40, Free: 60, CheckedAt: time.Now()}
	s := storageView{db: db, cached: cached}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	value, err := s.snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	got := value.(storageSnapshot)
	if got.Capacity != cached || got.Usage != nil || !strings.Contains(got.UsageError, "time limit") {
		t.Fatalf("capacity lost or partial usage reported: %+v", got)
	}
	raw, err := json.Marshal(value)
	if err != nil || strings.Contains(string(raw), `"usage":`) {
		t.Fatalf("partial usage must be omitted: %s, %v", raw, err)
	}
	value, err = s.snapshot(context.Background(), "")
	got = value.(storageSnapshot)
	if err != nil || got.UsageError != "" || got.Usage == nil || got.Usage.LocalBytes != 42 {
		t.Fatalf("failed to recover: %+v, %v", got, err)
	}
}
