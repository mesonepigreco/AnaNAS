package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsValid(t *testing.T) {
	cfg := Default()
	cfg.Local.Root = "/tmp/root"
	cfg.NAS.MountPoint = "/mnt/nas"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
	if cfg.Coalesce.Idle.Std() != 1500*time.Millisecond || cfg.Coalesce.MaxWait.Std() != 5*time.Second {
		t.Fatalf("unexpected coalesce defaults: %+v", cfg.Coalesce)
	}
	if !cfg.LANGuard {
		t.Fatalf("LAN guard should default to on")
	}
	if cfg.Remote.Enabled {
		t.Fatalf("remote should default to disabled")
	}
	if cfg.BlockSize != 64*1024 {
		t.Fatalf("unexpected block size %d", cfg.BlockSize)
	}
	if cfg.WebPort != 0 {
		t.Fatalf("web UI should be opt-in by default, got port %d", cfg.WebPort)
	}
}

func TestLoadMissingFileYieldsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LANGuard != true {
		t.Fatalf("defaults expected")
	}
}

func TestLoadOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{
	  "local":  {"root": "/home/me/docs"},
	  "nas":    {"host": "10.23.42.50", "share": "nas-sync", "mountPoint": "/mnt/nas-sync"},
	  "remote": {"enabled": true, "host": "nas.example.com", "port": 22,
	             "user": "example-user", "keyFile": "/home/me/.ssh/id_ed25519",
	             "knownHosts": "/home/me/.ssh/known_hosts"},
	  "coalesce": {"idle": "500ms", "maxWait": "3s"},
	  "selectiveSync": {"excludeLocal": ["node_modules/", "*.tmp"],
	                    "excludeRemote": ["cache/"]}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Local.Root != "/home/me/docs" {
		t.Fatalf("overlay failed on local.root: %q", cfg.Local.Root)
	}
	if cfg.NAS.Host != "10.23.42.50" || cfg.NAS.MountPoint != "/mnt/nas-sync" {
		t.Fatalf("overlay failed on nas: %+v", cfg.NAS)
	}
	if !cfg.Remote.Enabled || cfg.Remote.User != "example-user" || cfg.Remote.Port != 22 {
		t.Fatalf("overlay failed on remote: %+v", cfg.Remote)
	}
	if cfg.Coalesce.Idle.Std() != 500*time.Millisecond || cfg.Coalesce.MaxWait.Std() != 3*time.Second {
		t.Fatalf("overlay failed on coalesce: %+v", cfg.Coalesce)
	}
	if len(cfg.Selective.ExcludeLocal) != 2 || cfg.Selective.ExcludeLocal[0] != "node_modules/" {
		t.Fatalf("overlay failed on selective: %+v", cfg.Selective)
	}
	// Unset fields keep defaults.
	if cfg.BlockSize != 64*1024 {
		t.Fatalf("blockSize should keep default")
	}
}

func TestValidationErrors(t *testing.T) {
	base := Default()
	base.Local.Root = "/tmp/x"
	base.NAS.MountPoint = "/mnt/nas"

	cases := map[string]func(*Config){
		"missing local root":  func(c *Config) { c.Local.Root = "" },
		"missing mount point": func(c *Config) { c.NAS.MountPoint = "" },
		"bad protocol":        func(c *Config) { c.NAS.Protocol = "webdav" },
		"idle zero":           func(c *Config) { c.Coalesce.Idle = 0 },
		"maxWait less than idle": func(c *Config) {
			c.Coalesce.Idle = Duration(2 * time.Second)
			c.Coalesce.MaxWait = Duration(time.Second)
		},
		"tick beyond idle": func(c *Config) {
			c.Coalesce.Idle = Duration(100 * time.Millisecond)
			c.Coalesce.Tick = Duration(200 * time.Millisecond)
		},
		"bad block size":    func(c *Config) { c.BlockSize = 1234 },
		"port out of range": func(c *Config) { c.WebPort = 70000 },
	}
	for name, mutate := range cases {
		cfg := *base
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}

	// Remote enabled but incomplete.
	cfg := *base
	cfg.Remote = Remote{Enabled: true, Host: "nas.example.com"}
	if err := cfg.Validate(); err == nil {
		t.Errorf("remote enabled without keyFile should fail")
	}
}

func TestEnvDefaultPath(t *testing.T) {
	t.Setenv("NAS_SYNC_CONFIG", "/tmp/custom.json")
	if got := DefaultPath(); got != "/tmp/custom.json" {
		t.Fatalf("env override ignored: %q", got)
	}
}
