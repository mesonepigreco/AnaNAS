package main

import (
	"context"
	"nas-sync/internal/config"
	"testing"
)

func TestStorageDoesNotReportLocalDiskWhenNASUnmounted(t *testing.T) {
	nas := config.NAS{MountPoint: t.TempDir(), Host: "10.23.42.30", Share: "Nasdir", Protocol: "smb"}
	s := storageView{nas: func() config.NAS { return nas }}
	c := s.space(context.Background())
	if c.Available || c.Total != 0 || c.Message == "" {
		t.Fatalf("local disk reported as NAS: %+v", c)
	}
}
