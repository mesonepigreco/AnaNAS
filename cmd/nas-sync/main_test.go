package main

import (
	"context"
	"nas-sync/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func TestResolvedStateOverlapRejectedBeforeWriting(t *testing.T) {
	base := t.TempDir()
	local := filepath.Join(base, "local")
	if err := os.Mkdir(local, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(local, alias); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Local.Root = local
	cfg.NAS.MountPoint = "/mnt/unmounted"
	cfg.StateDir = filepath.Join(alias, "state")
	if err := runObserver(context.Background(), cfg, true); err == nil {
		t.Fatal("symlinked state inside root accepted")
	}
	if _, err := os.Stat(filepath.Join(local, "state")); !os.IsNotExist(err) {
		t.Fatalf("created state before checking alias: %v", err)
	}
}
