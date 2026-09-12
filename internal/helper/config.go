// Package helper assembles the native NAS transfer service. It is separate from
// the PC observer and never accesses a client-side CIFS/GVFS path.
package helper

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/config"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

type Config struct {
	LiveWrites         bool              `json:"liveWrites"`
	ObserverStateDir   string            `json:"observerStateDir,omitempty"`
	Root               string            `json:"root"`
	StateDir           string            `json:"stateDir"`
	Namespace          string            `json:"namespace"`
	Listen             string            `json:"listen"`
	Interface          string            `json:"interface"`
	Prefix             string            `json:"prefix"`
	Peers              []string          `json:"peers"`
	UID                int               `json:"uid"`
	GID                int               `json:"gid"`
	Groups             []int             `json:"groups"`
	Certificate        string            `json:"certificate"`
	PrivateKey         string            `json:"privateKey"`
	ClientCA           string            `json:"clientCA"`
	Clients            map[string]string `json:"clients"`
	Exclusions         []string          `json:"exclusions"`
	MaxConnections     int               `json:"maxConnections"`
	MaxFileBytes       int64             `json:"maxFileBytes"`
	MaxBatchBytes      int64             `json:"maxBatchBytes"`
	ReadBytesPerSecond int64             `json:"readBytesPerSecond"`
	MaxCacheBytes      int64             `json:"maxCacheBytes"`
	MaxCacheEntries    int64             `json:"maxCacheEntries"`
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}

func (c Config) Validate() error {
	if !cleanAbsolute(c.Root) || !cleanAbsolute(c.StateDir) || config.Overlap(c.Root, c.StateDir) || !manifest.ValidID(c.Namespace) || c.UID <= 0 || c.GID <= 0 {
		return fmt.Errorf("canonical disjoint native roots, namespace and non-root identity required")
	}
	if c.ObserverStateDir != "" && (!c.LiveWrites || !cleanAbsolute(c.ObserverStateDir) || config.Overlap(c.Root, c.ObserverStateDir) || config.Overlap(c.StateDir, c.ObserverStateDir)) {
		return fmt.Errorf("native observation requires live writes and a separate canonical state directory")
	}
	if len(c.Groups) > 32 {
		return fmt.Errorf("bounded supplementary groups required")
	}
	groupSet := make(map[int]bool, len(c.Groups))
	for _, group := range c.Groups {
		if group <= 0 || groupSet[group] {
			return fmt.Errorf("invalid supplementary groups")
		}
		groupSet[group] = true
	}
	endpoint, err := netip.ParseAddrPort(c.Listen)
	if err != nil || !endpoint.Addr().Is4() || endpoint.Port() == 0 || (!endpoint.Addr().IsPrivate() && !endpoint.Addr().IsLoopback()) {
		return fmt.Errorf("specific private IPv4 listener required")
	}
	prefix, err := netip.ParsePrefix(c.Prefix)
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || !prefix.Contains(endpoint.Addr()) {
		return fmt.Errorf("matching canonical IPv4 prefix required")
	}
	if c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/\\\x00 \t\r\n") || (endpoint.Addr().IsLoopback() != (c.Interface == "lo")) {
		return fmt.Errorf("explicit matching network interface required")
	}
	if len(c.Peers) < 1 || len(c.Peers) > 64 {
		return fmt.Errorf("one to 64 explicit peers required")
	}
	seen := make(map[netip.Addr]bool, len(c.Peers))
	for _, peer := range c.Peers {
		addr, err := netip.ParseAddr(peer)
		if err != nil || !addr.Is4() || !prefix.Contains(addr) || addr.IsLoopback() != endpoint.Addr().IsLoopback() || (!addr.IsPrivate() && !addr.IsLoopback()) || seen[addr] {
			return fmt.Errorf("invalid, duplicate or off-prefix peer")
		}
		seen[addr] = true
	}
	if len(c.Clients) < 1 || len(c.Clients) > 64 {
		return fmt.Errorf("bounded client certificate allowlist required")
	}
	for pin, client := range c.Clients {
		if !manifest.ValidID(pin) || !manifest.ValidID(client) {
			return fmt.Errorf("invalid certificate/client identity")
		}
	}
	for _, path := range []string{c.Certificate, c.PrivateKey, c.ClientCA} {
		if !cleanAbsolute(path) || config.Overlap(c.Root, path) {
			return fmt.Errorf("canonical TLS files outside the visible root required")
		}
	}
	if c.Certificate == c.PrivateKey || c.ClientCA == c.PrivateKey {
		return fmt.Errorf("private key must be separate from certificates")
	}
	if c.MaxConnections < 1 || c.MaxConnections > 8 || c.MaxFileBytes < 1 || c.MaxFileBytes > 8<<30 || c.MaxBatchBytes < c.MaxFileBytes || c.MaxBatchBytes > 8<<30 || c.ReadBytesPerSecond < 1024 || c.ReadBytesPerSecond > 1<<30 {
		return fmt.Errorf("bounded helper resource settings required")
	}
	if err := (journal.PublicationBudget{MaxBytes: c.MaxCacheBytes, MaxEntries: c.MaxCacheEntries}).Validate(); err != nil {
		return err
	}
	_, err = changefeed.Patterns(c.Exclusions)
	return err
}

// readPrivate opens every component without symlinks and reads only a bounded,
// owned private regular native file. It never enumerates a directory or logs its
// contents. Config and TLS material are not read through network/FUSE mounts.
func readPrivate(path string, maximum int64) ([]byte, error) {
	if !cleanAbsolute(path) {
		return nil, fmt.Errorf("canonical private file required")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if i != len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size < 1 || st.Size > maximum {
		return nil, fmt.Errorf("private file type, ownership, mode, link count or size differs")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return nil, err
	}
	switch uint64(uint32(fs.Type)) {
	case unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.SMB_SUPER_MAGIC, unix.NFS_SUPER_MAGIC, unix.FUSE_SUPER_MAGIC:
		return nil, fmt.Errorf("private files require native storage")
	}
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, err
	}
	if int64(len(data)) != st.Size || after.Size != st.Size || after.Mtim != st.Mtim || after.Ctim != st.Ctim {
		return nil, fmt.Errorf("private file changed while reading")
	}
	return data, nil
}

func Load(path string) (Config, error) {
	var c Config
	data, err := readPrivate(path, 64<<10)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("one helper configuration object required")
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	if config.Overlap(path, c.Root) {
		return c, fmt.Errorf("helper config must be outside visible root")
	}
	return c, nil
}

func (c Config) CheckIdentity() error {
	if os.Getuid() != c.UID || os.Geteuid() != c.UID || os.Getgid() != c.GID || os.Getegid() != c.GID || c.UID <= 0 || c.GID <= 0 {
		return fmt.Errorf("helper requires the configured non-admin UID/GID")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return err
	}
	expected := make(map[int]bool, len(c.Groups))
	for _, group := range c.Groups {
		expected[group] = true
	}
	actual := make(map[int]bool, len(groups))
	for _, group := range groups {
		actual[group] = true
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("helper has unexpected supplementary groups")
	}
	for group := range actual {
		if !expected[group] {
			return fmt.Errorf("helper has unexpected supplementary groups")
		}
	}
	return nil
}

func (c Config) TLS() (*tls.Config, error) {
	pair, pool, err := ReadIdentity(c.Certificate, c.PrivateKey, c.ClientCA)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	endpoint, _ := netip.ParseAddrPort(c.Listen)
	if err := leaf.VerifyHostname(endpoint.Addr().String()); err != nil {
		return nil, fmt.Errorf("helper certificate does not match listener IP")
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("helper certificate is not currently valid")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, NextProtos: []string{"http/1.1"}}, nil
}

// ReadIdentity loads only bounded, owned native private TLS files. It does not
// read system credential stores, contact an authority or print key contents.
func ReadIdentity(certificatePath, keyPath, caPath string) (tls.Certificate, *x509.CertPool, error) {
	certificate, err := readPrivate(certificatePath, 64<<10)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	key, err := readPrivate(keyPath, 16<<10)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pair, err := tls.X509KeyPair(certificate, key)
	clear(key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("invalid certificate/key pair: %w", err)
	}
	ca, err := readPrivate(caPath, 64<<10)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return tls.Certificate{}, nil, fmt.Errorf("invalid CA")
	}
	return pair, pool, nil
}
