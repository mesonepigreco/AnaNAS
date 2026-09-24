package nasfind

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelocateFindsOnlyThePinnedHost(t *testing.T) {
	moved := netip.MustParseAddr("10.23.42.77")
	var mu sync.Mutex
	probed := map[netip.Addr]int{}
	l, err := New(Options{Initial: netip.MustParseAddr("10.23.42.30"), Prefix: netip.MustParsePrefix("10.23.42.0/24"),
		Exclude: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("10.23.42.17")} },
		Probe: func(_ context.Context, a netip.Addr) error {
			mu.Lock()
			probed[a]++
			mu.Unlock()
			if a == moved {
				return nil
			}
			return errors.New("not the NAS")
		}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Relocate(context.Background(), netip.MustParseAddr("10.23.42.30"))
	if err != nil || got != moved || l.Current() != moved {
		t.Fatal(got, err)
	}
	for _, never := range []string{"10.23.42.0", "10.23.42.255", "10.23.42.17"} {
		if probed[netip.MustParseAddr(never)] != 0 {
			t.Fatal("probed", never)
		}
	}
	// A caller still holding the old address learns the new one without a search.
	if got, err := l.Relocate(context.Background(), netip.MustParseAddr("10.23.42.30")); err != nil || got != moved {
		t.Fatal(got, err)
	}
}

func TestRelocateIsRateLimitedAndSharesOneSearch(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	l, err := New(Options{Initial: netip.MustParseAddr("10.23.42.30"), Prefix: netip.MustParsePrefix("10.23.42.24/29"), Interval: time.Hour,
		Probe: func(context.Context, netip.Addr) error { calls.Add(1); <-release; return errors.New("absent") }})
	if err != nil {
		t.Fatal(err)
	}
	failed := netip.MustParseAddr("10.23.42.30")
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); l.Relocate(context.Background(), failed) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	first := calls.Load()
	if first == 0 || first > 6 {
		t.Fatal("unexpected probe count", first)
	}
	if _, err := l.Relocate(context.Background(), failed); err == nil || calls.Load() != first {
		t.Fatal("search repeated inside the interval", err)
	}
	if l.Current() != failed {
		t.Fatal("address changed without a verified probe")
	}
}

func TestNewRejectsWidePrefix(t *testing.T) {
	probe := func(context.Context, netip.Addr) error { return nil }
	if _, err := New(Options{Initial: netip.MustParseAddr("10.23.42.30"), Prefix: netip.MustParsePrefix("10.23.0.0/16"), Probe: probe}); err == nil {
		t.Fatal("wide prefix accepted")
	}
	if _, err := New(Options{Initial: netip.MustParseAddr("10.23.43.30"), Prefix: netip.MustParsePrefix("10.23.42.0/24"), Probe: probe}); err == nil {
		t.Fatal("initial address outside the prefix accepted")
	}
}
