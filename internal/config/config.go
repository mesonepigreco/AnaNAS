// Package config loads and validates the nas-sync configuration.
//
// Configuration is a single JSON file. All fields have defaults (see Default()
// in config.go); only the values the user wants to override need to be present.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nas-sync/internal/exclude"
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
	// Host is the configured NAS address. Automatic access requires an IP literal
	// and a verified direct route; a hostname never triggers background DNS.
	Host      string `json:"host"`
	Interface string `json:"interface"`
	Prefix    string `json:"prefix"`
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
	// Tick is deprecated and ignored; a deadline timer now drives coalescing.
	Tick       Duration `json:"tick"`
	MaxPending int      `json:"maxPending"`
	MaxBytes   int      `json:"maxBytes"`
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

// Limits bound background work. Hash/transfer settings are reserved for the
// later transfer engine; local observation uses ScanOpsPerSecond and MaxWatches.
type Limits struct {
	ReadBytesPerSecond int64 `json:"readBytesPerSecond"`
	ScanOpsPerSecond   int   `json:"scanOpsPerSecond"`
	MaxWatches         int   `json:"maxWatches"`
	CacheBytes         int64 `json:"cacheBytes"`
}

// Config is the effective, fully-defaulted configuration.
type Config struct {
	// LANGuard enforces that automatic sync only ever touches a LAN mount.
	LANGuard bool   `json:"lanGuard"`
	StateDir string `json:"stateDir"`
	Limits   Limits `json:"limits"`
	// ScanRemote is how often out-of-band remote changes are probed in LAN mode.
	ScanRemote Duration `json:"scanRemote"`
	BlockSize  int      `json:"blockSize"`
	// WebPort is the loopback-only status UI port. Zero disables the UI.
	WebPort   int           `json:"webPort"`
	NAS       NAS           `json:"nas"`
	Remote    Remote        `json:"remote"`
	Local     Local         `json:"local"`
	Coalesce  Coalesce      `json:"coalesce"`
	Selective SelectiveSync `json:"selectiveSync"`
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
		ScanRemote: Duration(15 * time.Minute),
		BlockSize:  64 * 1024, // 64 KiB content blocks (BLAKE3 digests)
		WebPort:    0,
		Limits:     Limits{ReadBytesPerSecond: 20 << 20, ScanOpsPerSecond: 50, MaxWatches: 100000, CacheBytes: 1 << 30},
		NAS: NAS{
			Protocol: "smb",
		},
		Remote: Remote{
			Enabled: false,
			Port:    22,
		},
		Coalesce: Coalesce{
			Idle:       Duration(1500 * time.Millisecond),
			MaxWait:    Duration(5 * time.Second),
			MaxPending: 10000,
			MaxBytes:   4 << 20,
		},
	}
}

// Load reads the file at path (if present) and overlays it on the defaults.
// A missing file is not an error: it yields the defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	const maxConfigBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("config exceeds 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("config must contain exactly one JSON object")
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

	if _, err := exclude.Compile(append(append([]string{}, c.Selective.ExcludeLocal...), c.Selective.ExcludeRemote...)); err != nil {
		return fmt.Errorf("selectiveSync: %w", err)
	}
	if !c.LANGuard {
		return fmt.Errorf("lanGuard cannot be disabled: automatic synchronization must remain LAN-only")
	}
	if !filepath.IsAbs(c.Local.Root) || !filepath.IsAbs(c.NAS.MountPoint) {
		return fmt.Errorf("local.root and nas.mountPoint must be absolute")
	}
	if Overlap(c.Local.Root, c.NAS.MountPoint) {
		return fmt.Errorf("local.root and nas.mountPoint must not overlap")
	}
	if c.StateDir != "" && (!filepath.IsAbs(c.StateDir) || Overlap(c.StateDir, c.Local.Root) || Overlap(c.StateDir, c.NAS.MountPoint)) {
		return fmt.Errorf("stateDir must be absolute and outside both roots")
	}
	if c.ScanRemote.Std() <= 0 || c.ScanRemote.Std() > 7*24*time.Hour {
		return fmt.Errorf("scanRemote must be in (0, 168h]")
	}
	if c.Coalesce.MaxPending < 1 || c.Coalesce.MaxPending > 100000 {
		return fmt.Errorf("coalesce.maxPending must be in [1, 100000]")
	}
	if c.Coalesce.MaxBytes < 4096 || c.Coalesce.MaxBytes > 64<<20 {
		return fmt.Errorf("coalesce.maxBytes must be in [4096, 67108864]")
	}
	if c.Coalesce.MaxWait.Std() > time.Hour {
		return fmt.Errorf("coalesce.maxWait must be <= 1h")
	}
	if c.Limits.ReadBytesPerSecond < 1024 || c.Limits.ReadBytesPerSecond > 1<<30 {
		return fmt.Errorf("limits.readBytesPerSecond must be in [1024, 1073741824]")
	}
	if c.Limits.ScanOpsPerSecond < 1 || c.Limits.ScanOpsPerSecond > 10000 {
		return fmt.Errorf("limits.scanOpsPerSecond must be in [1, 10000]")
	}
	if c.Limits.MaxWatches < 1 || c.Limits.MaxWatches > 1000000 {
		return fmt.Errorf("limits.maxWatches must be in [1, 1000000]")
	}
	if c.Limits.CacheBytes < 1<<20 || c.Limits.CacheBytes > 1<<40 {
		return fmt.Errorf("limits.cacheBytes must be in [1048576, 1099511627776]")
	}
	if c.NAS.Prefix != "" {
		prefix, err := netip.ParsePrefix(c.NAS.Prefix)
		if err != nil {
			return fmt.Errorf("nas.prefix must be a CIDR prefix")
		}
		addr, err := netip.ParseAddr(c.NAS.Host)
		if err != nil || !prefix.Contains(addr) {
			return fmt.Errorf("nas.host must be an IP address within nas.prefix")
		}
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
	if c.Coalesce.Tick.Std() < 0 || c.Coalesce.Tick.Std() > c.Coalesce.Idle.Std() {
		return fmt.Errorf("coalesce.tick must be in [0, coalesce.idle]")
	}
	if c.BlockSize < 1<<10 || c.BlockSize > 4<<20 || c.BlockSize&(c.BlockSize-1) != 0 {
		return fmt.Errorf("blockSize must be a power of two in [1024, 4194304]")
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

// Overlap performs a lexical check; runtime code must also check resolved paths.
func Overlap(a, b string) bool {
	within := func(root, p string) bool {
		rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
		return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
	}
	return within(a, b) || within(b, a)
}
