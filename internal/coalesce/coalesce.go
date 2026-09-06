// Package coalesce merges bursts of file-system events into a single commit
// unit ("chunk") so that a constantly-writing process produces one block-diff
// rather than thousands (PLAN.md §5.3). It is the implementation of
// PROJECT.md requirement 7.
//
// Semantics:
//   - The batch opens on the first event after a flush.
//   - A quiet period of Idle since the last event closes the batch.
//   - A continuously-active batch is force-closed after MaxWait from its start.
//
// The policy itself lives in ShouldFlush (pure, unit-testable). Run wires it to
// an event channel using a lightweight ticker that is only armed while events
// are pending, so idle cost is zero.
package coalesce

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Config tunes the coalescing behaviour.
type Config struct {
	Idle    time.Duration // quiet time after the last event that closes the batch
	MaxWait time.Duration // hard cap on batch lifetime while events keep arriving
	Tick    time.Duration // evaluation granularity; must be > 0
}

// Validate returns an error if the configuration is unusable.
func (c Config) Validate() error {
	if c.Idle <= 0 {
		return fmt.Errorf("idle must be positive")
	}
	if c.MaxWait < c.Idle {
		return fmt.Errorf("maxWait must be >= idle")
	}
	if c.Tick <= 0 || c.Tick > c.Idle {
		return fmt.Errorf("tick must be in (0, idle]")
	}
	return nil
}

// ShouldFlush is the pure coalescing decision.
//
// batchOpen reports whether any event is currently pending; start/last are the
// times of the first and last event of the open batch; now is the evaluation
// time. A flush is due when the batch has been quiet for idle, or when it has
// lived for maxWait even under continuous churn.
func ShouldFlush(batchOpen bool, start, last, now time.Time, idle, maxWait time.Duration) bool {
	if !batchOpen {
		return false
	}
	return now.Sub(last) >= idle || now.Sub(start) >= maxWait
}

// Coalescer merges incoming path events and emits batches of unique paths.
type Coalescer struct {
	cfg Config
}

// New returns a Coalescer. The Config is validated; invalid values panic, so
// callers should Validate() first if they accept user input.
func New(cfg Config) *Coalescer {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return &Coalescer{cfg: cfg}
}

// Run processes events from in and sends a batch of unique, sorted paths on out
// whenever the coalescing window closes. It returns when ctx is cancelled or in
// is closed (flushing any pending batch before closing out).
func (c *Coalescer) Run(ctx context.Context, in <-chan string) <-chan []string {
	out := make(chan []string)
	go c.run(ctx, in, out)
	return out
}

func (c *Coalescer) run(ctx context.Context, in <-chan string, out chan<- []string) {
	defer close(out)

	var pending map[string]struct{}
	var start, last time.Time
	var ticker *time.Ticker
	var tickC <-chan time.Time

	flush := func() {
		if len(pending) == 0 {
			return
		}
		paths := make([]string, 0, len(pending))
		for p := range pending {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		pending = nil
		stopTicker(ticker)
		ticker, tickC = nil, nil
		select {
		case out <- paths:
		case <-ctx.Done():
		}
	}

	arm := func() {
		if ticker == nil {
			ticker = time.NewTicker(c.cfg.Tick)
			tickC = ticker.C
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-in:
			if !ok {
				flush()
				return
			}
			if pending == nil {
				pending = make(map[string]struct{})
				start = time.Now()
			}
			last = time.Now()
			pending[ev] = struct{}{}
			arm()
		case <-tickC:
			now := time.Now()
			if ShouldFlush(pending != nil, start, last, now, c.cfg.Idle, c.cfg.MaxWait) {
				flush()
			}
		}
	}
}

func stopTicker(t *time.Ticker) {
	if t != nil {
		t.Stop()
	}
}
