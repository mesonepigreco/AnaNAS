// Package socketpolicy applies native IPv4 socket constraints before network I/O.
// These checks complement, rather than replace, deployment egress validation.
package socketpolicy

import (
	"fmt"
	"net"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func ipv4TCP(fd int, device string) error {
	if device == "" || len(device) >= unix.IFNAMSIZ || strings.ContainsAny(device, "/\\\x00 \t\r\n") {
		return fmt.Errorf("explicit socket device required")
	}
	for _, option := range []struct{ key, want int }{{unix.SO_DOMAIN, unix.AF_INET}, {unix.SO_TYPE, unix.SOCK_STREAM}, {unix.SO_PROTOCOL, unix.IPPROTO_TCP}} {
		value, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, option.key)
		if err != nil || value != option.want {
			return fmt.Errorf("native IPv4 TCP socket required")
		}
	}
	return nil
}

// BindIPv4 applies and reads back device binding and SO_DONTROUTE. It must run
// before bind/listen/connect, including before a TCP handshake. No fallback is
// allowed if the kernel rejects or fails to retain either option.
func BindIPv4(fd int, device string) error {
	if err := ipv4TCP(fd, device); err != nil {
		return err
	}
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device); err != nil {
		return err
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DONTROUTE, 1); err != nil {
		return err
	}
	return VerifyIPv4(fd, device)
}

func VerifyIPv4(fd int, device string) error {
	if err := ipv4TCP(fd, device); err != nil {
		return err
	}
	actual, err := unix.GetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
	if err != nil || strings.TrimRight(actual, "\x00") != device {
		return fmt.Errorf("socket device binding differs")
	}
	direct, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DONTROUTE)
	if err != nil || direct != 1 {
		return fmt.Errorf("socket must prohibit gateway routing")
	}
	return nil
}

// VerifyConn checks inherited accepted sockets without changing them. The
// listener's flags must survive kernel cloning before application/TLS I/O.
func VerifyConn(conn net.Conn, device string) error {
	c, ok := conn.(syscall.Conn)
	if !ok {
		return fmt.Errorf("native TCP connection required")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var policyErr error
	if err := raw.Control(func(fd uintptr) { policyErr = VerifyIPv4(int(fd), device) }); err != nil {
		return err
	}
	return policyErr
}
