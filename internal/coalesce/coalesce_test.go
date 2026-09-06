package coalesce

import (
	"context"
	"testing"
	"time"
)

func TestShouldFlush(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	idle := 2 * time.Second
	maxWait := 5 * time.Second

	cases := []struct {
		name      string
		open      bool
		start, lt time.Time // start and last-event times
		now       time.Time
		want      bool
	}{
		{"empty batch never flushes", false, base, base, base.Add(10 * time.Second), false},
		{"recent event, within idle", true, base, base.Add(500 * time.Millisecond), base.Add(time.Second), false},
		{"quiet since idle", true, base, base, base.Add(idle), true},
		{"quiet beyond idle", true, base, base.Add(1 * time.Second), base.Add(4 * time.Second), true},
		{"continuous churn still capped by maxWait", true, base, base.Add(4*time.Second + 900*time.Millisecond), base.Add(4*time.Second + 900*time.Millisecond), false},
		{"maxWait reached under churn", true, base, base.Add(5 * time.Second), base.Add(5 * time.Second), true},
	}
	for _, tc := range cases {
		got := ShouldFlush(tc.open, tc.start, tc.lt, tc.now, idle, maxWait)
		if got != tc.want {
			t.Errorf("%s: ShouldFlush=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	good := Config{Idle: time.Second, MaxWait: 2 * time.Second, Tick: 50 * time.Millisecond}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
	bad := []Config{
		{Idle: 0, MaxWait: time.Second, Tick: 10 * time.Millisecond},
		{Idle: 2 * time.Second, MaxWait: time.Second, Tick: 10 * time.Millisecond},
		{Idle: time.Second, MaxWait: 2 * time.Second, Tick: 2 * time.Second},
		{Idle: -time.Second, MaxWait: time.Second, Tick: 10 * time.Millisecond},
	}
	for _, cfg := range bad {
		if err := cfg.Validate(); err == nil {
			t.Errorf("config %+v should be rejected", cfg)
		}
	}
}

// collect drains a batch channel until it closes, returning every path.
func collect(ctx context.Context, ch <-chan Batch) [][]string {
	var batches [][]string
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				return batches
			}
			batches = append(batches, b.Paths)
		case <-ctx.Done():
			return batches
		}
	}
}

func TestRunQuietBurstIsOneBatch(t *testing.T) {
	cfg := Config{Idle: 80 * time.Millisecond, MaxWait: 400 * time.Millisecond, Tick: 10 * time.Millisecond}
	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan string)
	out := c.Run(ctx, in)

	go func() {
		for i := 0; i < 300; i++ {
			in <- "file.txt"
		}
		close(in)
	}()

	batches := collect(ctx, out)
	if len(batches) != 1 {
		t.Fatalf("quiet burst produced %d batches, want 1", len(batches))
	}
	if got := batches[0]; len(got) != 1 || got[0] != "file.txt" {
		t.Fatalf("unexpected batch content: %v", got)
	}
}

func TestRunContinuousChurnCappedByMaxWait(t *testing.T) {
	cfg := Config{Idle: 100 * time.Millisecond, MaxWait: 250 * time.Millisecond, Tick: 10 * time.Millisecond}
	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan string)
	out := c.Run(ctx, in)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Events every ~15ms beat Idle=100ms, so only MaxWait can close a batch.
		for i := 0; i < 80; i++ {
			in <- "busy.log"
			time.Sleep(15 * time.Millisecond)
		}
		close(in)
	}()

	var batchCount int
	for b := range out {
		batchCount++
		if len(b.Paths) != 1 || b.Paths[0] != "busy.log" {
			t.Fatalf("unexpected batch %v", b)
		}
	}
	<-done

	// 80 events, 15ms apart => ~1.2s of churn. With Idle=100ms beaten by the
	// write rate, MaxWait=250ms should force flushes => ~5 batches, not 80.
	if batchCount < 2 || batchCount > 20 {
		t.Fatalf("churn produced %d batches, expected a handful (~5)", batchCount)
	}
}

func TestRunNoEventsEmitsNothing(t *testing.T) {
	cfg := Config{Idle: 30 * time.Millisecond, MaxWait: 100 * time.Millisecond, Tick: 5 * time.Millisecond}
	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan string)
	out := c.Run(ctx, in)
	close(in)

	if got := collect(ctx, out); len(got) != 0 {
		t.Fatalf("expected no batches for empty input, got %d", len(got))
	}
}
