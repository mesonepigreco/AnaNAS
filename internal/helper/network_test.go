package helper

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

func routeReply(kind byte, device uint32, attribute uint16) []byte {
	raw := make([]byte, unix.NLMSG_HDRLEN+unix.SizeofRtMsg+8)
	binary.NativeEndian.PutUint16(raw[4:6], unix.RTM_NEWROUTE)
	binary.NativeEndian.PutUint32(raw[8:12], 1)
	raw[16], raw[23] = unix.AF_INET, kind
	at := unix.NLMSG_HDRLEN + unix.SizeofRtMsg
	binary.NativeEndian.PutUint16(raw[at:at+2], 8)
	binary.NativeEndian.PutUint16(raw[at+2:at+4], unix.RTA_OIF)
	binary.NativeEndian.PutUint32(raw[at+4:at+8], device)
	if attribute != 0 {
		a := make([]byte, 8)
		binary.NativeEndian.PutUint16(a[:2], 8)
		binary.NativeEndian.PutUint16(a[2:4], attribute)
		raw = append(raw, a...)
	}
	binary.NativeEndian.PutUint32(raw[:4], uint32(len(raw)))
	return raw
}

func TestHelperRouteRejectsGatewayAndAmbiguousEvidence(t *testing.T) {
	if err := validateRouteReply(routeReply(unix.RTN_UNICAST, 2, 0), 2, false); err != nil {
		t.Fatal(err)
	}
	if err := validateRouteReply(routeReply(unix.RTN_LOCAL, 1, 0), 1, true); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{routeReply(unix.RTN_UNICAST, 2, unix.RTA_GATEWAY), routeReply(unix.RTN_UNICAST, 2, unix.RTA_MULTIPATH), routeReply(unix.RTN_UNICAST, 2, unix.RTA_VIA), routeReply(unix.RTN_UNICAST, 3, 0), routeReply(unix.RTN_LOCAL, 2, 0), routeReply(unix.RTN_BLACKHOLE, 2, 0), []byte{1, 2, 3}} {
		if err := validateRouteReply(raw, 2, false); err == nil {
			t.Fatal("unsafe route accepted")
		}
	}
}
