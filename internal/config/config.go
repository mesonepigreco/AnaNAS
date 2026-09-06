// Package config loads and validates the nas-sync configuration.
//
// Configuration is a single JSON file. All fields have defaults (see defaults()
// in config.go); only the values the user wants to override need to be present.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Duration is a time.Duration that JSON-marshals as a string such as "1.5s"
// (time.ParseDuration) while still accepting a plain integer of nanoseconds.
type Duration time.Duration

// Std converts back to time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the duration in Go syntax.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalJSON accepts "1.5s", "1500ms", or a bare nanosecond integer.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*d = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		pd, err := time.ParseDuration(str)
		if err != nil {
			return err
		}
		*d = Duration(pd)
		return nil
	}
	var ns int64
	if err := json.Unmarshal(b, &ns); err != nil {
		return fmt.Errorf("duration must be a string like %q or nanoseconds: %s", "1.5s", b)
	}
	*d = Duration(ns)
	return nil
}

// MarshalJSON renders the duration as a string for readability.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// NAS describes the QNAP share this client synchronizes with.
type NAS struct {
	// Host is the LAN IP or hostname of the NAS. Only private-address
	// resolutions are accepted by the LAN guard (internal/languard).
	Host string `json:"host"`
	// Share is the SMB/NFS share name (informational).
	Share string `json:"share"`
	// MountPoint is where the share is already mounted by the system
	// (e.g. /mnt/nas-sync). nas-sync never mounts as root itself.
	MountPoint string `json:"mountPoint"`
	// Protocol is "smb" or "nfs"; used only for hints and docs.
	Protocol string `json:"protocol"`
}

// Local describes the directory that is watched and mirrored to the NAS.
type Local struct {
	// Root is the local directory to synchronize.
	Root string `json:"root"`
}

// Coalesce tunes how bursts of file-system events are merged into a single
// commit ("chunk"). See PLAN.md §5.3.
type Coalesce struct {
	// Idle is the quiet period after the last event that closes a batch.
	Idle Duration `json:"idle"`
	// MaxWait caps how long a continuously-active batch may grow.
	MaxWait Duration `json:"maxWait"`
	// Tick is how often the coalescer evaluates Idle/MaxWait.
	Tick Duration `json:"tick"`
}

// SelectiveSync holds gitignore-style exclusion globs.
type SelectiveSync struct {
	// ExcludeLocal: paths/patterns that are never watched or uploaded.
	ExcludeLocal []string `json:"excludeLocal"`
	// ExcludeRemote: remote paths/patterns that are never downloaded or tracked.
	ExcludeRemote []string `json:"excludeRemote"`
}

// Remote describes the off-LAN SFTP endpoint used for on-demand access only.
// It is never used for automatic sync: see PLAN.md §5.2.
type Remote struct {
	Enabled    bool   `json:"enabled"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	KeyFile    string `json:"keyFile"`
	KnownHosts string `json:"knownHosts"`
}

// Config is the effective, fully-defaulted configuration.
type Config struct {
	// LANGuard enforces that automatic sync only ever touches a LAN mount.
	LANGuard bool `json:"lanGuard"`
	// ScanRemote is how often out-of-band remote changes are probed in LAN mode.
	ScanRemote Duration      `json:"scanRemote"`
	BlockSize  int           `json:"blockSize"`
	WebPort    int           `json:"webPort"`
	NAS        NAS           `json:"nas"`
	Remote     Remote        `json:"remote"`
	Local      Local         `json:"local"`
	Coalesce   Coalesce      `json:"coalesce"`
	Selective  SelectiveSync `json:"selectiveSync"`
}

// DefaultPath returns the conventional configuration file location.
func DefaultPath() string {
	if p := os.Getenv("NAS_SYNC_CONFIG"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "nas-sync", "config.json")
	}
	return "nas-sync.json"
}

// Default returns the built-in defaults, merged over later by file values.
func Default() *Config {
	return &Config{
		LANGuard:   true,
		ScanRemote: Duration(60 * time.Second),
		BlockSize:  64 * 1024, // 64 KiB content blocks (BLAKE3 digests)
		WebPort:    8721,
		NAS: NAS{
			Protocol: "smb",
		},
		Remote: Remote{
			Enabled: false,
			Port:    22,
		},
		Coalesce: Coalesce{
			Idle:    Duration(1500 * time.Millisecond),
			MaxWait: Duration(5 * time.Second),
			Tick:    Duration(50 * time.Millisecond),
		},
	}
}

// Load reads the file at path (if present) and overlays it on the defaults.
// A missing file is not an error: it yields the defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate performs structural checks only (no I/O). Existence of directories
// is checked by the engine at runtime so that a NAS that is temporarily
// unmounted never prevents the config from loading.
func (c *Config) Validate() error {
	if c.Local.Root == "" {
		return fmt.Errorf("local.root is required")
	}
	if c.NAS.MountPoint == "" {
		return fmt.Errorf("nas.mountPoint is required")
	}
	if c.Remote.Enabled {
		if c.Remote.Host == "" || c.Remote.User == "" {
			return fmt.Errorf("remote.host and remote.user are required when remote is enabled")
		}
		if c.Remote.Port <= 0 || c.Remote.Port > 65535 {
			return fmt.Errorf("remote.port out of range")
		}
		if c.Remote.KeyFile == "" || c.Remote.KnownHosts == "" {
			return fmt.Errorf("remote.keyFile and remote.knownHosts are required when remote is enabled")
		}
	}
	if c.Coalesce.Idle.Std() <= 0 {
		return fmt.Errorf("coalesce.idle must be positive")
	}
	if c.Coalesce.MaxWait.Std() < c.Coalesce.Idle.Std() {
		return fmt.Errorf("coalesce.maxWait must be >= coalesce.idle")
	}
	if c.Coalesce.Tick.Std() <= 0 || c.Coalesce.Tick.Std() > c.Coalesce.Idle.Std() {
		return fmt.Errorf("coalesce.tick must be in (0, coalesce.idle]")
	}
	if c.BlockSize < 1<<10 || c.BlockSize&(c.BlockSize-1) != 0 {
		return fmt.Errorf("blockSize must be a power of two >= 1024")
	}
	if c.WebPort < 0 || c.WebPort > 65535 {
		return fmt.Errorf("webPort out of range")
	}
	switch c.NAS.Protocol {
	case "smb", "nfs", "":
	default:
		return fmt.Errorf("nas.protocol must be smb or nfs")
	}
	return nil
}

// Print writes the effective configuration as indented JSON.
func Print(w io.Writer, c *Config) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(c)
}
