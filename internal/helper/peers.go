package helper

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ParsePeer accepts one client IPv4 address or a canonical client subnet. A
// subnet peer keeps a DHCP-addressed PC authorized after its lease changes; the
// pinned client certificate remains the authentication boundary and every
// accepted connection is still checked for a direct route to its actual address.
func ParsePeer(value string) (netip.Prefix, error) {
	if strings.Contains(value, "/") {
		p, err := netip.ParsePrefix(value)
		if err != nil || p != p.Masked() {
			return netip.Prefix{}, fmt.Errorf("canonical peer subnet required")
		}
		return p, nil
	}
	a, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// InterfaceSource returns the single IPv4 address that device currently holds
// inside prefix. It reads only local interface metadata, so a DHCP renumbering
// is followed on the next policy check instead of pinning a stale address.
func InterfaceSource(device string, prefix netip.Prefix) (netip.Addr, error) {
	nic, err := net.InterfaceByName(device)
	if err != nil {
		return netip.Addr{}, err
	}
	addresses, err := nic.Addrs()
	if err != nil {
		return netip.Addr{}, err
	}
	var found []netip.Addr
	for _, address := range addresses {
		p, err := netip.ParsePrefix(address.String())
		if err == nil && p.Addr().Is4() && p.Masked() == prefix {
			found = append(found, p.Addr())
		}
	}
	switch len(found) {
	case 0:
		return netip.Addr{}, fmt.Errorf("permitted interface %s has no address in %s", device, prefix)
	case 1:
		return found[0], nil
	default:
		return netip.Addr{}, fmt.Errorf("permitted interface %s has several addresses in %s", device, prefix)
	}
}
