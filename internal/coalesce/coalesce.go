// Package coalesce merges events into bounded batches without blocking event
// ingestion on a slow consumer. Overflow requests reconciliation, never deletion.
package coalesce

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const DefaultMaxPending = 10000
const DefaultMaxBytes = 4 << 20

type Config struct {
	Idle, MaxWait time.Duration
	// Tick is accepted for configuration compatibility; deadline timers replace it.
	Tick       time.Duration
	MaxPending int
	MaxBytes   int
}

func (c Config) Validate() error {
	if c.Idle <= 0 {
		return fmt.Errorf("idle must be positive")
	}
	if c.MaxWait < c.Idle {
		return fmt.Errorf("maxWait must be >= idle")
	}
	if c.Tick < 0 || c.Tick > c.Idle {
		return fmt.Errorf("tick must be in [0, idle]")
	}
	if c.MaxPending < 0 || c.MaxPending > 100000 {
		return fmt.Errorf("maxPending must be in [0, 100000]")
	}
	if c.MaxBytes < 0 || c.MaxBytes > 64<<20 {
		return fmt.Errorf("maxBytes must be in [0, 67108864]")
	}
	return nil
}

func ShouldFlush(open bool, start, last, now time.Time, idle, maxWait time.Duration) bool {
	return open && (now.Sub(last) >= idle || now.Sub(start) >= maxWait)
}

type Batch struct {
	Paths []string
	// Rescan supersedes Paths. The consumer must reconcile the root; paths were
	// deliberately collapsed after exceeding the memory budget or an event gap.
	Rescan bool
}

// RescanPath is reserved for producers that report watcher overflow.
const RescanPath = "."

type Coalescer struct{ cfg Config }

func New(cfg Config) *Coalescer {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	if cfg.MaxPending == 0 {
		cfg.MaxPending = DefaultMaxPending
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	return &Coalescer{cfg}
}

// Run drains input while a batch is waiting for its consumer. At most two bounded
// batches exist. Input close flushes remaining work; cancellation discards it, so
// persistent consumers must reconcile on restart to cover shutdown event gaps.
func (c *Coalescer) Run(ctx context.Context, in <-chan string) <-chan Batch {
	out := make(chan Batch)
	go c.run(ctx, in, out)
	return out
}

func (c *Coalescer) run(ctx context.Context, in <-chan string, out chan<- Batch) {
	defer close(out)
	var timer *time.Timer
	var alarm <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	pending := make(map[string]struct{})
	var start, last time.Time
	var bytes int
	var overflow, due bool
	var ready *Batch
	stop := func() {
		if timer != nil {
			timer.Stop()
		}
		alarm = nil
	}
	arm := func() {
		deadline := minTime(last.Add(c.cfg.Idle), start.Add(c.cfg.MaxWait))
		if timer == nil {
			timer = time.NewTimer(time.Until(deadline))
		} else {
			timer.Reset(time.Until(deadline))
		}
		alarm = timer.C
	}
	for {
		if ready == nil && due {
			b := Batch{Rescan: overflow}
			if !overflow {
				b.Paths = make([]string, 0, len(pending))
				for p := range pending {
					b.Paths = append(b.Paths, p)
				}
				sort.Strings(b.Paths)
			}
			ready = &b
			clear(pending)
			bytes = 0
			overflow = false
			due = false
			start = time.Time{}
			stop()
		}
		if in == nil && ready == nil && start.IsZero() {
			return
		}
		var send chan<- Batch
		var value Batch
		if ready != nil {
			send = out
			value = *ready
		}
		select {
		case <-ctx.Done():
			return
		case send <- value:
			ready = nil
		case ev, ok := <-in:
			if !ok {
				in = nil
				stop()
				due = !start.IsZero()
				continue
			}
			now := time.Now()
			if start.IsZero() {
				start = now
			}
			last = now
			if ev == RescanPath {
				overflow = true
				clear(pending)
				bytes = 0
			}
			if !overflow {
				if _, exists := pending[ev]; !exists {
					if len(pending) >= c.cfg.MaxPending || len(ev) > c.cfg.MaxBytes-bytes {
						overflow = true
						clear(pending)
						bytes = 0
					} else {
						pending[ev] = struct{}{}
						bytes += len(ev)
					}
				}
			}
			if !due {
				arm()
			}
		case <-alarm:
			due = true
			stop()
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
