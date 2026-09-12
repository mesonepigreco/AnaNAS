package socketpolicy

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestSocketPolicyRejectsMissingOrChangedFlags(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := VerifyIPv4(fd, "lo"); err == nil {
		t.Fatal("unbound socket accepted")
	}
	if err := BindIPv4(fd, "lo"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyIPv4(fd, "lo"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyIPv4(fd, "different"); err == nil {
		t.Fatal("wrong device accepted")
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DONTROUTE, 0); err != nil {
		t.Fatal(err)
	}
	if err := VerifyIPv4(fd, "lo"); err == nil {
		t.Fatal("gateway-enabled socket accepted")
	}
	for _, device := range []string{"", "lo\x00", "lo\n", "sixteenbytesname!"} {
		if err := BindIPv4(fd, device); err == nil {
			t.Fatal("invalid device accepted", device)
		}
	}
}

func TestSocketPolicyRejectsOtherProtocolFamilies(t *testing.T) {
	for _, test := range []struct{ family, kind, protocol int }{{unix.AF_INET6, unix.SOCK_STREAM, unix.IPPROTO_TCP}, {unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP}, {unix.AF_UNIX, unix.SOCK_STREAM, 0}} {
		fd, err := unix.Socket(test.family, test.kind|unix.SOCK_CLOEXEC, test.protocol)
		if err != nil {
			t.Fatal(err)
		}
		err = BindIPv4(fd, "lo")
		unix.Close(fd)
		if err == nil {
			t.Fatal("non-IPv4/TCP socket accepted", test)
		}
	}
}
