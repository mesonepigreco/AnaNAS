package helper

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"nas-sync/internal/socketpolicy"
)

type peerKey struct{}

// InheritedListener accepts only a prebound IPv4 listening socket on the exact
// configured address and device. A narrow privileged launcher can bind it before
// dropping to the dedicated UID; the service never gains bind capabilities or
// creates an outbound connection. Unrelated/invalid descriptors are not closed.
func InheritedListener(fd int, c Config) (net.Listener, error) {
	if fd < 3 {
		return nil, fmt.Errorf("inherited listener descriptor must be at least 3")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	kind, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || kind != unix.SOCK_STREAM {
		return nil, fmt.Errorf("inherited TCP stream socket required")
	}
	listening, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil || listening != 1 {
		return nil, fmt.Errorf("inherited socket must already be listening")
	}
	if err := socketpolicy.VerifyIPv4(fd, c.Interface); err != nil {
		return nil, err
	}
	address, err := unix.Getsockname(fd)
	if err != nil {
		return nil, err
	}
	v4, ok := address.(*unix.SockaddrInet4)
	if !ok {
		return nil, fmt.Errorf("IPv4 listener required")
	}
	actual := netip.AddrPortFrom(netip.AddrFrom4(v4.Addr), uint16(v4.Port))
	if actual.String() != c.Listen {
		return nil, fmt.Errorf("inherited listener address differs")
	}
	f := os.NewFile(uintptr(fd), "ananas-listener")
	defer f.Close()
	listener, err := net.FileListener(f)
	if err != nil {
		return nil, err
	}
	return &directListener{Listener: listener, device: c.Interface}, nil
}

type directListener struct {
	net.Listener
	device string
}

func (l *directListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if err := socketpolicy.VerifyConn(conn, l.device); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// CheckNetwork reads local interface and exact source-bound route evidence only.
// It does not ping, resolve DNS, list content or open an outbound socket. These
// checks detect changed policy but are NOT a replacement for OS egress rules.
func CheckNetwork(ctx context.Context, c Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	endpoint, err := netip.ParseAddrPort(c.Listen)
	if err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(c.Prefix)
	if err != nil {
		return err
	}
	nic, err := net.InterfaceByName(c.Interface)
	if err != nil {
		return err
	}
	if nic.Flags&net.FlagUp == 0 || nic.Flags&net.FlagPointToPoint != 0 {
		return fmt.Errorf("helper interface is down or point-to-point")
	}
	loopback := endpoint.Addr().IsLoopback()
	if loopback {
		if nic.Flags&net.FlagLoopback == 0 || c.Interface != "lo" {
			return fmt.Errorf("loopback test interface required")
		}
	} else {
		if nic.Flags&net.FlagLoopback != 0 {
			return fmt.Errorf("physical helper interface required")
		}
		if _, err := os.Stat(filepath.Join("/sys/class/net", c.Interface, "device")); err != nil {
			return fmt.Errorf("helper interface lacks physical-device evidence")
		}
		carrier, err := os.ReadFile(filepath.Join("/sys/class/net", c.Interface, "carrier"))
		if err != nil || strings.TrimSpace(string(carrier)) != "1" {
			return fmt.Errorf("helper interface lacks carrier")
		}
	}
	addresses, err := nic.Addrs()
	if err != nil {
		return err
	}
	found := false
	for _, address := range addresses {
		p, err := netip.ParsePrefix(address.String())
		if err == nil && p.Addr() == endpoint.Addr() && p.Masked() == prefix {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("helper address/prefix is no longer on the permitted interface")
	}
	peers := c.Peers
	if peer, ok := ctx.Value(peerKey{}).(netip.Addr); ok {
		peers = []string{peer.String()}
	}
	for _, peer := range peers {
		address, err := netip.ParseAddr(peer)
		if err != nil || !address.Is4() || !prefix.Contains(address) {
			return fmt.Errorf("peer is outside the direct subnet")
		}
		if err := directRoute(ctx, endpoint.Addr(), address, nic.Index, loopback); err != nil {
			return err
		}
	}
	return nil
}

func directRoute(ctx context.Context, source, destination netip.Addr, ifindex int, loopback bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	timeout := 200 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	tv := unix.NsecToTimeval(int64(timeout))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	// One RTM_GETROUTE with explicit source, destination and bound output device.
	// There is no table dump, subprocess, DNS lookup or network probe.
	request := make([]byte, unix.NLMSG_HDRLEN+unix.SizeofRtMsg)
	binary.NativeEndian.PutUint16(request[4:6], unix.RTM_GETROUTE)
	binary.NativeEndian.PutUint16(request[6:8], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(request[8:12], 1)
	request[16], request[17], request[18] = unix.AF_INET, 32, 32
	attribute := func(kind uint16, data []byte) {
		header := make([]byte, 4)
		binary.NativeEndian.PutUint16(header[:2], uint16(4+len(data)))
		binary.NativeEndian.PutUint16(header[2:], kind)
		request = append(append(request, header...), data...)
	}
	src, dst := source.As4(), destination.As4()
	attribute(unix.RTA_SRC, src[:])
	attribute(unix.RTA_DST, dst[:])
	var dev [4]byte
	binary.NativeEndian.PutUint32(dev[:], uint32(ifindex))
	attribute(unix.RTA_OIF, dev[:])
	binary.NativeEndian.PutUint32(request[:4], uint32(len(request)))
	if err := unix.Sendto(fd, request, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buffer := make([]byte, 8192)
	n, from, err := unix.Recvfrom(fd, buffer, 0)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	kernel, ok := from.(*unix.SockaddrNetlink)
	if !ok || kernel.Pid != 0 || n == len(buffer) {
		return fmt.Errorf("unexpected or oversized route response")
	}
	return validateRouteReply(buffer[:n], ifindex, loopback)
}

func validateRouteReply(raw []byte, ifindex int, loopback bool) error {
	messages, err := syscall.ParseNetlinkMessage(raw)
	if err != nil {
		return err
	}
	if len(messages) != 1 {
		return fmt.Errorf("ambiguous route response")
	}
	m := messages[0]
	if m.Header.Seq != 1 || m.Header.Flags&unix.NLM_F_MULTI != 0 {
		return fmt.Errorf("unexpected route response ordering")
	}
	if m.Header.Type == unix.NLMSG_ERROR {
		return fmt.Errorf("kernel rejected source-bound route lookup")
	}
	if m.Header.Type != unix.RTM_NEWROUTE || len(m.Data) < unix.SizeofRtMsg || m.Data[0] != unix.AF_INET || (m.Data[7] != unix.RTN_UNICAST && !(loopback && m.Data[7] == unix.RTN_LOCAL)) {
		return fmt.Errorf("route is not permitted unicast/local traffic")
	}
	attributes, err := syscall.ParseNetlinkRouteAttr(&m)
	if err != nil {
		return err
	}
	device := false
	for _, a := range attributes {
		switch a.Attr.Type {
		case unix.RTA_GATEWAY, unix.RTA_MULTIPATH, unix.RTA_VIA:
			return fmt.Errorf("route uses a gateway, multipath or alternate family")
		case unix.RTA_OIF:
			if device || len(a.Value) != 4 || binary.NativeEndian.Uint32(a.Value) != uint32(ifindex) {
				return fmt.Errorf("route output device differs")
			}
			device = true
		}
	}
	if !device {
		return fmt.Errorf("route lacks output device evidence")
	}
	return nil
}
