package netwatch

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func message(kind uint16, family byte, index uint32) []byte {
	data := make([]byte, 16)
	data[0] = family
	binary.NativeEndian.PutUint32(data[4:8], index)
	raw := make([]byte, unix.NLMSG_HDRLEN+len(data))
	binary.NativeEndian.PutUint32(raw[:4], uint32(len(raw)))
	binary.NativeEndian.PutUint16(raw[4:6], kind)
	copy(raw[unix.NLMSG_HDRLEN:], data)
	return raw
}

func TestNetworkEventsIdentifyRelevantStateAndRejectUncertainty(t *testing.T) {
	for _, tc := range []struct {
		kind   uint16
		family byte
		index  uint32
		want   bool
	}{
		{unix.RTM_NEWLINK, unix.AF_UNSPEC, 4, true}, {unix.RTM_DELLINK, unix.AF_UNSPEC, 4, true},
		{unix.RTM_NEWLINK, unix.AF_UNSPEC, 5, false},
		{unix.RTM_NEWADDR, unix.AF_INET, 4, true}, {unix.RTM_DELADDR, unix.AF_INET, 4, true},
		{unix.RTM_NEWADDR, unix.AF_INET, 5, false}, {unix.RTM_NEWADDR, unix.AF_INET6, 4, false},
		{unix.RTM_NEWROUTE, unix.AF_INET, 5, true}, {unix.RTM_DELROUTE, unix.AF_INET, 5, true},
		{unix.RTM_NEWRULE, unix.AF_INET, 5, true}, {unix.RTM_DELRULE, unix.AF_INET, 5, true},
		{unix.RTM_NEWROUTE, unix.AF_INET6, 4, false},
		{unix.RTM_NEWNEXTHOP, unix.AF_UNSPEC, 0, true}, {unix.RTM_DELNEXTHOP, unix.AF_INET6, 0, true},
	} {
		got, err := relevant(message(tc.kind, tc.family, tc.index), 4)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, raw := range [][]byte{{0}, message(unix.NLMSG_ERROR, 0, 0), message(unix.NLMSG_OVERRUN, 0, 0), message(65535, 0, 0), message(unix.RTM_NEWROUTE, 255, 0), message(unix.RTM_NEWADDR, 255, 4)} {
		if _, err := relevant(raw, 4); !errors.Is(err, ErrUncertain) {
			t.Fatal("uncertainty ignored", err)
		}
	}
	valid := message(unix.RTM_NEWLINK, 0, 4)
	if _, err := relevant(append(valid, 1), 4); !errors.Is(err, ErrUncertain) {
		t.Fatal("malformed tail ignored", err)
	}
	if _, err := relevant(valid[:len(valid)-1], 4); !errors.Is(err, ErrUncertain) {
		t.Fatal("truncated event ignored", err)
	}
	batch := append(message(unix.RTM_NEWLINK, 0, 5), valid...)
	if changed, err := relevant(batch, 4); err != nil || !changed {
		t.Fatal("relevant later message missed", changed, err)
	}
}

func TestKernelSubscriptionCancelsAndClosesDescriptors(t *testing.T) {
	run := func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready, done := make(chan struct{}, 1), make(chan error, 1)
		go func() { done <- (Monitor{Interface: "lo"}).Watch(ctx, ready) }()
		select {
		case <-ready:
		case err := <-done:
			t.Fatal("kernel subscription failed", err)
		case <-time.After(time.Second):
			t.Fatal("subscription did not become ready")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation did not wake poll")
		}
	}
	run() // warm up the test process before descriptor accounting
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		run()
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil || len(after) != len(before) {
		t.Fatal("monitor leaked descriptors", len(before), len(after), err)
	}
}

func TestMonitorRejectsInvalidAndCanceledStartup(t *testing.T) {
	for _, name := range []string{"", "bad/name", "name with spaces", "0123456789abcdef"} {
		if err := (Monitor{Interface: name}).Watch(context.Background(), make(chan struct{}, 1)); err == nil {
			t.Fatal("invalid interface accepted", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (Monitor{Interface: "lo"}).Watch(ctx, make(chan struct{}, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMissingInterfaceWaitsWithoutPolling(t *testing.T) {
	// A name known absent by the same exact local ioctl used by the monitor.
	const name = "ananas-absent"
	if _, err := interfaceIndex(name); !errors.Is(err, unix.ENODEV) {
		t.Fatal("fixture interface must be absent", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, done := make(chan struct{}, 1), make(chan error, 1)
	go func() { done <- (Monitor{Interface: name}).Watch(ctx, ready) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("missing interface did not subscribe")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop")
	}
	if changed, err := relevant(message(unix.RTM_NEWLINK, unix.AF_UNSPEC, 12), 0); err != nil || !changed {
		t.Fatal("missing interface must re-resolve on link creation", changed, err)
	}
}
