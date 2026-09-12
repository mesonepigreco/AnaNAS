// Package netwatch consumes local Linux routing notifications. It sends no IP
// traffic and neither authorizes a route nor replaces the socket egress policy.
package netwatch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrChanged = errors.New("network configuration changed; revalidation required")
var ErrUncertain = errors.New("network notification state uncertain; revalidation required")

const bufferBytes = 32 << 10
const receiveBytes = 64 << 10
const maxIgnoredDatagrams = 64

type Monitor struct{ Interface string }

// Watch subscribes before resolving the exact interface index, then sends one
// readiness signal. It returns on the first relevant change or uncertainty. The
// caller cancels/joins transfer services and revalidates for the next session.
// Cancellation wakes a blocking poll through eventfd, without a polling timer.
// The monitor owns and closes all descriptors; callers join Watch before exit.
func (m Monitor) Watch(ctx context.Context, ready chan<- struct{}) error {
	if m.Interface == "" || len(m.Interface) >= unix.IFNAMSIZ || strings.ContainsAny(m.Interface, "/\\\x00 \t\r\n") || cap(ready) != 1 {
		return fmt.Errorf("explicit interface and one-slot readiness channel required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, receiveBytes); err != nil {
		return err
	}
	actual, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF)
	if err != nil {
		return err
	}
	if actual <= 0 || actual > 2*receiveBytes {
		return fmt.Errorf("bounded network receive buffer unavailable")
	}
	groups := uint32(unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV4_RULE)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		return err
	}
	// Next-hop objects can change routing without replacing the referencing
	// route. Unsupported subscription fails closed; this is a PC-kernel path.
	if err := unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_ADD_MEMBERSHIP, unix.RTNLGRP_NEXTHOP); err != nil {
		return err
	}
	index, err := interfaceIndex(m.Interface)
	if err != nil && !errors.Is(err, unix.ENODEV) {
		return err
	}
	// An absent interface waits for any link event, without offline polling.
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return err
	}
	defer unix.Close(wake)
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		var one [8]byte
		binary.NativeEndian.PutUint64(one[:], 1)
		for {
			_, err := unix.Write(wake, one[:])
			if !errors.Is(err, unix.EINTR) {
				return
			}
		}
	})
	// Join a started cancellation callback before its descriptor can be closed
	// and reused. No callback/goroutine remains after Watch returns.
	defer func() {
		if !stop() {
			<-cancelDone
		}
	}()
	select {
	case ready <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}}
	buffer := make([]byte, bufferBytes)
	for ignored := 0; ignored < maxIgnoredDatagrams; {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := unix.Poll(poll, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if poll[1].Revents != 0 {
			return fmt.Errorf("%w: unexpected cancellation descriptor event", ErrUncertain)
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return fmt.Errorf("%w: notification descriptor error", ErrUncertain)
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			return fmt.Errorf("%w: unexpected readiness", ErrUncertain)
		}
		n, _, flags, from, err := unix.Recvmsg(fd, buffer, nil, unix.MSG_DONTWAIT)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: receive: %v", ErrUncertain, err)
		}
		peer, ok := from.(*unix.SockaddrNetlink)
		if !ok || peer.Pid != 0 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n <= 0 || n > len(buffer) {
			return fmt.Errorf("%w: source or truncation", ErrUncertain)
		}
		changed, err := relevant(buffer[:n], index)
		if err != nil {
			return err
		}
		if changed {
			return ErrChanged
		}
		ignored++
	}
	return fmt.Errorf("%w: unrelated notification budget exhausted", ErrUncertain)
}

// SIOCGIFINDEX reads one exact interface; the unconnected control socket sends
// no packet and avoids dumping/enumerating every interface on the machine.
func interfaceIndex(name string) (uint32, error) {
	request, err := unix.NewIfreq(name)
	if err != nil {
		return 0, err
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFINDEX, request); err != nil {
		return 0, err
	}
	index := request.Uint32()
	if index == 0 || index > 1<<31-1 {
		return 0, fmt.Errorf("invalid interface index")
	}
	return index, nil
}

func relevant(raw []byte, index uint32) (bool, error) {
	changed := false
	for len(raw) > 0 {
		if len(raw) < unix.NLMSG_HDRLEN {
			return false, fmt.Errorf("%w: short message", ErrUncertain)
		}
		n := int(binary.NativeEndian.Uint32(raw[:4]))
		if n < unix.NLMSG_HDRLEN || n > len(raw) {
			return false, fmt.Errorf("%w: invalid message length", ErrUncertain)
		}
		typeID := binary.NativeEndian.Uint16(raw[4:6])
		data := raw[unix.NLMSG_HDRLEN:n]
		switch typeID {
		case unix.NLMSG_NOOP, unix.NLMSG_DONE:
		case unix.RTM_NEWLINK, unix.RTM_DELLINK:
			if len(data) < unix.SizeofIfInfomsg {
				return false, ErrUncertain
			}
			changed = changed || index == 0 || binary.NativeEndian.Uint32(data[4:8]) == index
		case unix.RTM_NEWADDR, unix.RTM_DELADDR:
			if len(data) < unix.SizeofIfAddrmsg {
				return false, ErrUncertain
			}
			if data[0] != unix.AF_INET && data[0] != unix.AF_INET6 {
				return false, ErrUncertain
			}
			changed = changed || (data[0] == unix.AF_INET && binary.NativeEndian.Uint32(data[4:8]) == index)
		case unix.RTM_NEWROUTE, unix.RTM_DELROUTE, unix.RTM_NEWRULE, unix.RTM_DELRULE:
			if len(data) < unix.SizeofRtMsg {
				return false, ErrUncertain
			}
			if data[0] != unix.AF_INET && data[0] != unix.AF_INET6 {
				return false, ErrUncertain
			}
			changed = changed || data[0] == unix.AF_INET
		case unix.RTM_NEWNEXTHOP, unix.RTM_DELNEXTHOP, unix.RTM_NEWNEXTHOPBUCKET, unix.RTM_DELNEXTHOPBUCKET:
			if len(data) < 8 {
				return false, ErrUncertain
			}
			changed = true // includes group objects without an address family
		default:
			return false, fmt.Errorf("%w: unexpected notification type %d", ErrUncertain, typeID)
		}
		aligned := (n + 3) &^ 3
		if n == len(raw) {
			return changed, nil
		}
		if aligned > len(raw) {
			return false, ErrUncertain
		}
		raw = raw[aligned:]
	}
	return changed, nil
}
