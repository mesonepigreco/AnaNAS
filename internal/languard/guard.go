// Package languard checks local evidence without probing the NAS.
// Eligibility is diagnostic only: verified transport capabilities and OS egress
// enforcement are additionally required before enabling automatic writes.
package languard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"nas-sync/internal/config"
)

type Mount struct {
	Point, Source, Type string
	Options             []string
}
type Route struct {
	Dst     string `json:"dst"`
	Dev     string `json:"dev"`
	Gateway string `json:"gateway"`
	Type    string `json:"type"`
}
type Result struct {
	Eligible        bool   `json:"eligible"`
	Reason          string `json:"reason"`
	AutomaticWrites bool   `json:"automaticWrites"`
}

// DiscoverSystem returns network-looking mounts visible to this user. Kernel
// CIFS/NFS mounts come from mountinfo; desktop sessions often expose SMB shares
// as GVFS FUSE directories instead. GVFS entries are deliberately labelled
// separately because they do not provide the same locking, fsync or watch
// guarantees as a kernel CIFS mount.
func DiscoverSystem() ([]Mount, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	mounts, err := ParseMounts(raw)
	if err != nil {
		return nil, err
	}
	var network []Mount
	for _, m := range mounts {
		switch m.Type {
		case "cifs", "nfs", "nfs4", "sshfs", "fuse.sshfs":
			network = append(network, m)
		}
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	gvfsRoot := filepath.Join(runtimeDir, "gvfs")
	entries, readErr := os.ReadDir(gvfsRoot)
	if readErr != nil && !os.IsNotExist(readErr) {
		return network, readErr
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "smb-share:") {
			continue
		}
		server, share, ok := parseGVFSSMB(name)
		if !ok {
			continue
		}
		network = append(network, Mount{
			Point:  filepath.Join(gvfsRoot, name),
			Source: "//" + server + "/" + share,
			Type:   "gvfs-smb",
			Options: []string{
				"user-space",
				"server=" + server,
				"share=" + share,
			},
		})
	}
	return network, nil
}

func parseGVFSSMB(name string) (server, share string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(name, "smb-share:"), ",")
	for _, part := range parts {
		key, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return "", "", false
		}
		switch key {
		case "server":
			server = decoded
		case "share":
			share = decoded
		}
	}
	return server, share, server != "" && share != ""
}

func ParseMounts(data []byte) ([]Mount, error) {
	var mounts []Mount
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 4096), 1<<20)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) < sep+4 {
			return nil, fmt.Errorf("invalid mountinfo record")
		}
		mounts = append(mounts, Mount{Point: unescape.Replace(fields[4]), Source: unescape.Replace(fields[sep+2]), Type: fields[sep+1], Options: strings.Split(fields[5]+","+fields[sep+3], ",")})
	}
	return mounts, s.Err()
}

// Evaluate is pure and rejects ambiguous evidence, tunnels and private routes
// through gateways. A configured prefix is checked against interface addresses.
func Evaluate(n config.NAS, mounts []Mount, routes []Route, physical bool, addresses []netip.Prefix) Result {
	deny := func(s string) Result { return Result{Reason: s} }
	addr, err := netip.ParseAddr(n.Host)
	if err != nil || !addr.IsPrivate() {
		return deny("NAS host must be a private IP literal; no DNS probes are performed")
	}
	prefix, err := netip.ParsePrefix(n.Prefix)
	if err != nil || !prefix.Contains(addr) || n.Interface == "" {
		return deny("configure nas.interface and a prefix containing the NAS")
	}
	if !physical {
		return deny("permitted interface is not an active physical LAN interface")
	}
	connected := false
	for _, p := range addresses {
		if p.Contains(addr) && prefix.Contains(p.Addr()) && p.Addr() != addr {
			connected = true
		}
	}
	if !connected {
		return deny("NAS is not on the permitted interface's directly connected subnet")
	}
	if len(routes) != 1 || routes[0].Dev != n.Interface || routes[0].Gateway != "" || (routes[0].Type != "" && routes[0].Type != "unicast") {
		return deny("route is ambiguous, uses a gateway, or leaves the permitted interface")
	}
	var match *Mount
	for i := range mounts {
		m := &mounts[i]
		if filepath.Clean(m.Point) == filepath.Clean(n.MountPoint) {
			if match != nil {
				return deny("stacked mounts at NAS path require manual inspection")
			}
			match = m
		}
	}
	if match == nil {
		return deny("expected NAS share is not mounted")
	}
	if n.Protocol != "smb" || match.Type != "cifs" {
		if match.Type == "gvfs-smb" {
			return deny("GVFS SMB mount found; use a kernel CIFS mount for automatic synchronization (GVFS is inspection/test-only)")
		}
		return deny("only kernel CIFS SMB mounts are supported for automatic synchronization")
	}
	if match.Source != "//"+n.Host+"/"+n.Share || n.Share == "" {
		return deny("mount source does not match the configured NAS and share")
	}
	serverAddress := false
	for _, opt := range match.Options {
		if opt == "ro" {
			return deny("NAS mount is read-only; automatic synchronization writes are disabled")
		}
		if opt == "addr="+n.Host {
			serverAddress = true
		}
		if opt == "multichannel" || (strings.HasPrefix(opt, "max_channels=") && opt != "max_channels=1") {
			return deny("SMB multichannel routes are not verified")
		}
	}
	if !serverAddress {
		return deny("mount does not confirm the configured server address")
	}
	return Result{Eligible: true, Reason: "direct LAN mount/route evidence matches; writes remain disabled pending NAS capability and egress validation"}
}

// Inspect reads local kernel state and invokes `ip route get`, which performs a
// route lookup, not a ping or connection. It never stats the NAS/automount path.
func Inspect(ctx context.Context, n config.NAS) Result {
	fail := func(err error) Result { return Result{Reason: err.Error()} }
	if _, err := netip.ParseAddr(n.Host); err != nil {
		return fail(fmt.Errorf("configure a NAS IP literal; background DNS is disabled"))
	}
	mounts, err := DiscoverSystem()
	if err != nil {
		return fail(err)
	}
	nic, err := net.InterfaceByName(n.Interface)
	if err != nil {
		return fail(err)
	}
	_, err = os.Stat(filepath.Join("/sys/class/net", n.Interface, "device"))
	physical := err == nil && nic.Flags&net.FlagUp != 0 && nic.Flags&net.FlagLoopback == 0
	addrs, err := nic.Addrs()
	if err != nil {
		return fail(err)
	}
	var prefixes []netip.Prefix
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			prefixes = append(prefixes, p)
		}
	}
	raw, err := exec.CommandContext(ctx, "ip", "-j", "route", "get", n.Host).Output()
	if err != nil {
		return fail(fmt.Errorf("local route lookup: %w", err))
	}
	var routes []Route
	if err = json.Unmarshal(raw, &routes); err != nil {
		return fail(err)
	}
	return Evaluate(n, mounts, routes, physical, prefixes)
}
