package netwatch

import (
	"encoding/binary"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func addressMessage(kind uint16, ip byte, lifetime uint32) []byte {
	raw := message(kind, unix.AF_INET, 4)
	raw = raw[:unix.NLMSG_HDRLEN+unix.SizeofIfAddrmsg]
	raw[unix.NLMSG_HDRLEN+1] = 24
	attr := make([]byte, 8)
	binary.NativeEndian.PutUint16(attr, 8)
	binary.NativeEndian.PutUint16(attr[2:], unix.IFA_LOCAL)
	copy(attr[4:], []byte{192, 168, 1, ip})
	raw = append(raw, attr...)
	cache := make([]byte, 20)
	binary.NativeEndian.PutUint16(cache, 20)
	binary.NativeEndian.PutUint16(cache[2:], unix.IFA_CACHEINFO)
	binary.NativeEndian.PutUint32(cache[4:], lifetime)
	raw = append(raw, cache...)
	binary.NativeEndian.PutUint32(raw, uint32(len(raw)))
	return raw
}

func TestAddressRefreshDoesNotInterruptButPolicyChangesDo(t *testing.T) {
	initial := addressMessage(unix.RTM_NEWADDR, 18, 7000)
	key, err := addressKey(initial[unix.NLMSG_HDRLEN:])
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{key: true}
	refresh := addressMessage(unix.RTM_NEWADDR, 18, 6992)
	if raw, err := withoutAddressRefreshes(refresh, known); err != nil || len(raw) != 0 {
		t.Fatal("lease refresh interrupted sync", raw, err)
	}
	prefix := append([]byte(nil), initial...)
	prefix[unix.NLMSG_HDRLEN+1] = 25
	flags := append([]byte(nil), initial...)
	flags[unix.NLMSG_HDRLEN+2] = unix.IFA_F_DEPRECATED
	for _, change := range [][]byte{addressMessage(unix.RTM_NEWADDR, 19, 7000), addressMessage(unix.RTM_DELADDR, 18, 7000), prefix, flags, message(unix.RTM_NEWROUTE, unix.AF_INET, 4), message(unix.RTM_DELLINK, 0, 4)} {
		batch := append(append([]byte(nil), refresh...), change...)
		raw, err := withoutAddressRefreshes(batch, known)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := relevant(raw, 4); err != nil || !changed {
			t.Fatal("real change ignored", changed, err)
		}
	}
	if _, err := withoutAddressRefreshes(append(refresh, 1), known); !errors.Is(err, ErrUncertain) {
		t.Fatal("malformed tail ignored", err)
	}
}
