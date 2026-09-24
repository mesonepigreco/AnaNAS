// Package nasfind follows a NAS whose DHCP address may change. It never trusts
// an address by itself: a candidate is accepted only after the caller's probe
// proves the pinned NAS certificate on the helper port.
package nasfind

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// MaxHosts bounds one search to a /22 so a mistaken wide prefix cannot turn a
// reconnect into a large network scan.
const MaxHosts = 1022

// Probe returns nil only when address answers as the pinned NAS.
type Probe func(ctx context.Context, address netip.Addr) error

type Locator struct {
	prefix      netip.Prefix
	probe       Probe
	exclude     func() []netip.Addr
	interval    time.Duration
	concurrency int
	timeout     time.Duration

	mu       sync.Mutex
	current  netip.Addr
	searched time.Time
	search   chan struct{}
}

type Options struct {
	Initial netip.Addr
	Prefix  netip.Prefix
	Probe   Probe
	// Exclude lists addresses never probed, such as this PC's own address.
	Exclude func() []netip.Addr
	// Interval is the minimum time between searches (default one minute).
	Interval time.Duration
}

func New(o Options) (*Locator, error) {
	if !o.Prefix.IsValid() || !o.Prefix.Addr().Is4() || o.Prefix != o.Prefix.Masked() || !o.Prefix.Contains(o.Initial) || o.Probe == nil {
		return nil, fmt.Errorf("canonical IPv4 prefix containing the initial NAS address and a probe required")
	}
	if hosts(o.Prefix) > MaxHosts {
		return nil, fmt.Errorf("NAS search prefix is wider than /22")
	}
	if o.Interval <= 0 {
		o.Interval = time.Minute
	}
	return &Locator{prefix: o.Prefix, probe: o.Probe, exclude: o.Exclude, interval: o.Interval, concurrency: 32, timeout: 800 * time.Millisecond, current: o.Initial}, nil
}

func hosts(p netip.Prefix) int {
	return 1<<(32-p.Bits()) - 2
}

// Current returns the last verified (or initially configured) NAS address.
func (l *Locator) Current() netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current
}

// Relocate searches the direct prefix after failed stopped answering as the
// NAS. Concurrent callers share one search, and searches are rate limited.
func (l *Locator) Relocate(ctx context.Context, failed netip.Addr) (netip.Addr, error) {
	l.mu.Lock()
	if l.current != failed {
		current := l.current
		l.mu.Unlock()
		return current, nil
	}
	if wait := l.search; wait != nil {
		l.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		}
		return l.Current(), nil
	}
	if !l.searched.IsZero() && time.Since(l.searched) < l.interval {
		l.mu.Unlock()
		return failed, fmt.Errorf("NAS is not answering at %s; searching the LAN again shortly", failed)
	}
	done := make(chan struct{})
	l.search, l.searched = done, time.Now()
	l.mu.Unlock()
	found, err := l.scan(ctx, failed)
	l.mu.Lock()
	if err == nil {
		l.current = found
	}
	l.search = nil
	close(done)
	l.mu.Unlock()
	if err != nil {
		return failed, err
	}
	return found, nil
}

func (l *Locator) scan(parent context.Context, failed netip.Addr) (netip.Addr, error) {
	skip := map[netip.Addr]bool{}
	if l.exclude != nil {
		for _, a := range l.exclude() {
			skip[a] = true
		}
	}
	// Probe the previous address first: a brief outage should not wait on a
	// full sweep. Network and broadcast addresses are never probed.
	candidates := []netip.Addr{failed}
	last := netip.AddrFrom4(broadcast(l.prefix))
	for a := l.prefix.Addr().Next(); a.IsValid() && a != last; a = a.Next() {
		if a != failed && !skip[a] {
			candidates = append(candidates, a)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	work := make(chan netip.Addr)
	found := make(chan netip.Addr, 1)
	var wg sync.WaitGroup
	for range min(l.concurrency, len(candidates)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range work {
				probe, stop := context.WithTimeout(ctx, l.timeout)
				err := l.probe(probe, a)
				stop()
				if err == nil {
					select {
					case found <- a:
					default:
					}
					cancel()
				}
			}
		}()
	}
feed:
	for _, a := range candidates {
		select {
		case work <- a:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	select {
	case a := <-found:
		return a, nil
	default:
	}
	if err := parent.Err(); err != nil {
		return netip.Addr{}, err
	}
	return netip.Addr{}, fmt.Errorf("NAS with the pinned certificate was not found on %s", l.prefix)
}

func broadcast(p netip.Prefix) [4]byte {
	b := p.Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return [4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
