package main

import (
	"context"
	"nas-sync/internal/config"
	"testing"
)

func TestStorageDoesNotReportLocalDiskWhenNASUnmounted(t *testing.T) {
	s := storageView{cfg: config.NAS{MountPoint: t.TempDir(), Host: "10.23.42.30", Share: "Nasdir", Protocol: "smb"}}
	c := s.space(context.Background())
	if c.Available || c.Total != 0 || c.Message == "" {
		t.Fatalf("local disk reported as NAS: %+v", c)
	}
}
