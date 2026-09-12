package config

import (
	"fmt"
	"net/netip"
	"path/filepath"

	"nas-sync/internal/manifest"
)

// Sync configures the explicitly enabled native LAN trial. TLS identities are
// provisioned separately; disabled/default observation never opens these files.
type Sync struct {
	Enabled           bool   `json:"enabled"`
	Port              int    `json:"port"`
	Source            string `json:"source"`
	Namespace         string `json:"namespace"`
	ReplicaNamespace  string `json:"replicaNamespace"`
	Certificate       string `json:"certificate"`
	PrivateKey        string `json:"privateKey"`
	CA                string `json:"ca"`
	ServerFingerprint string `json:"serverFingerprint"`
	MaxFileBytes      int64  `json:"maxFileBytes"`
	MaxBatchBytes     int64  `json:"maxBatchBytes"`
	MaxCacheEntries   int64  `json:"maxCacheEntries"`
}

func (s Sync) validate(c *Config) error {
	if !s.Enabled {
		return nil
	}
	if !manifest.ValidID(s.Namespace) || !manifest.ValidID(s.ReplicaNamespace) || s.Namespace == s.ReplicaNamespace || !manifest.ValidID(s.ServerFingerprint) {
		return fmt.Errorf("sync requires distinct namespace identities and a server certificate pin")
	}
	ip, err := netip.ParseAddr(c.NAS.Host)
	if err != nil || !ip.Is4() || !ip.IsPrivate() {
		return fmt.Errorf("sync requires a private IPv4 NAS literal")
	}
	source, err := netip.ParseAddr(s.Source)
	if err != nil || !source.Is4() || !source.IsPrivate() || source == ip {
		return fmt.Errorf("sync requires a distinct private IPv4 source")
	}
	prefix, err := netip.ParsePrefix(c.NAS.Prefix)
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || !prefix.Contains(ip) || !prefix.Contains(source) || c.NAS.Interface == "" {
		return fmt.Errorf("sync requires an explicit interface and matching direct subnet")
	}
	if s.Port < 1024 || s.Port > 65535 || s.MaxFileBytes < 1 || s.MaxFileBytes > 8<<30 || s.MaxBatchBytes < s.MaxFileBytes || s.MaxBatchBytes > 8<<30 || s.MaxCacheEntries < 1 || s.MaxCacheEntries > 1_000_000 {
		return fmt.Errorf("sync requires bounded port, file, batch and cache entry settings")
	}
	for _, p := range []string{s.Certificate, s.PrivateKey, s.CA} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || Overlap(p, c.Local.Root) || Overlap(p, c.NAS.MountPoint) {
			return fmt.Errorf("sync TLS files must be canonical and outside both sync roots")
		}
	}
	return nil
}
