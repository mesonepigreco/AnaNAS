package netwatch

import (
	"encoding/binary"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Subscribe before taking this snapshot. A queued deletion or changed address
// still interrupts the session, even if a later event restores the old address.
func addressSnapshot(index uint32) (map[string]bool, error) {
	known := make(map[string]bool)
	if index == 0 {
		return known, nil
	}
	raw, err := syscall.NetlinkRIB(unix.RTM_GETADDR, unix.AF_INET)
	if err != nil {
		return nil, err
	}
	messages, err := syscall.ParseNetlinkMessage(raw)
	if err != nil {
		return nil, err
	}
	for _, m := range messages {
		if m.Header.Type != unix.RTM_NEWADDR || len(m.Data) < unix.SizeofIfAddrmsg || binary.NativeEndian.Uint32(m.Data[4:8]) != index {
			continue
		}
		key, err := addressKey(m.Data)
		if err != nil {
			return nil, err
		}
		known[key] = true
	}
	return known, nil
}

// Preserve the entire address description, including flags, scope, prefix,
// peer, broadcast and unknown attributes. Only the cache timestamps/lifetimes
// may differ: refreshing an existing lease does not change the LAN policy.
func addressKey(data []byte) (string, error) {
	if len(data) < unix.SizeofIfAddrmsg || data[0] != unix.AF_INET {
		return "", ErrUncertain
	}
	header := string(data[:unix.SizeofIfAddrmsg])
	data = data[unix.SizeofIfAddrmsg:]
	var attrs []string
	local := false
	for len(data) > 0 {
		if len(data) < 4 {
			return "", ErrUncertain
		}
		n := int(binary.NativeEndian.Uint16(data[:2]))
		kind := binary.NativeEndian.Uint16(data[2:4])
		if n < 4 || n > len(data) {
			return "", ErrUncertain
		}
		if kind == unix.IFA_LOCAL {
			if n != 8 {
				return "", ErrUncertain
			}
			local = true
		}
		if kind != unix.IFA_CACHEINFO {
			attrs = append(attrs, string(data[:n]))
		} else if n != 20 {
			return "", ErrUncertain
		}
		if n == len(data) {
			break
		}
		n = (n + 3) &^ 3
		if n > len(data) {
			return "", ErrUncertain
		}
		data = data[n:]
	}
	if !local {
		return "", ErrUncertain
	}
	sort.Strings(attrs)
	return header + strings.Join(attrs, ""), nil
}

func withoutAddressRefreshes(raw []byte, known map[string]bool) ([]byte, error) {
	var remaining []byte
	for len(raw) > 0 {
		if len(raw) < unix.NLMSG_HDRLEN {
			return nil, ErrUncertain
		}
		n := int(binary.NativeEndian.Uint32(raw[:4]))
		if n < unix.NLMSG_HDRLEN || n > len(raw) {
			return nil, ErrUncertain
		}
		refresh := false
		data := raw[unix.NLMSG_HDRLEN:n]
		if binary.NativeEndian.Uint16(raw[4:6]) == unix.RTM_NEWADDR && len(data) >= unix.SizeofIfAddrmsg && data[0] == unix.AF_INET {
			key, err := addressKey(data)
			if err != nil {
				return nil, err
			}
			refresh = known[key]
		}
		aligned := (n + 3) &^ 3
		if n == len(raw) {
			aligned = n
		}
		if aligned > len(raw) {
			return nil, ErrUncertain
		}
		if !refresh {
			remaining = append(remaining, raw[:aligned]...)
		}
		raw = raw[aligned:]
	}
	return remaining, nil
}
