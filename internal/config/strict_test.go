package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStrictJSON(t *testing.T) {
	for _, extra := range []string{`,"limitz":{}`, `,"coalesce":{"idle":"1s","typo":1}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(`{"local":{"root":"/tmp/local"},"nas":{"mountPoint":"/mnt/nas"}`+extra+`}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatal("unknown field accepted")
		}
	}
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{} {}`), 0600)
	if _, err := Load(p); err == nil {
		t.Fatal("trailing document accepted")
	}
}
func TestResourceAndRootValidation(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.LANGuard = false }, func(c *Config) { c.ScanRemote = 0 },
		func(c *Config) { c.BlockSize = 1 << 30 }, func(c *Config) { c.Coalesce.MaxPending = 0 },
		func(c *Config) { c.Coalesce.MaxBytes = 1 }, func(c *Config) { c.Limits.ScanOpsPerSecond = 0 },
		func(c *Config) { c.Local.Root = "relative" }, func(c *Config) { c.NAS.MountPoint = "/tmp/local/child" },
		func(c *Config) { c.StateDir = "/tmp/local/state" }, func(c *Config) { c.NAS.Prefix = "10.23.42.0/24"; c.NAS.Host = "10.0.0.1" },
	} {
		c := Default()
		c.Local.Root = "/tmp/local"
		c.NAS.MountPoint = "/mnt/nas"
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}
