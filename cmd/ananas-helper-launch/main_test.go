package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSupervisorRecoversFailuresAndCleanExitsWithoutOverlappingAttempts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts, waiting := 0, false
	var delays []time.Duration
	err := supervise(ctx, launchOptions{}, func() error {
		if waiting {
			t.Fatal("attempt overlapped retry wait")
		}
		attempts++
		if attempts == 10 {
			cancel() // The replacement helper ran until shutdown.
			return context.Canceled
		}
		waiting = true
		if attempts%2 == 0 {
			return nil // An unexpected clean exit also needs recovery.
		}
		return errors.New("temporary startup or child failure")
	}, func(_ context.Context, delay time.Duration) bool {
		if !waiting {
			t.Fatal("retry without joining the previous attempt")
		}
		delays = append(delays, delay)
		waiting = false
		return true
	})
	if err != nil || attempts != 10 || len(delays) != 9 {
		t.Fatal(attempts, delays, err)
	}
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60} {
		if delays[i] != want*time.Second {
			t.Fatal("unbounded or incorrect retry delay", delays)
		}
	}
}

func TestSupervisorPreservesBoundedRuns(t *testing.T) {
	failure := errors.New("child failed")
	for _, o := range []launchOptions{{write: true}, {lifetime: time.Minute}} {
		for _, result := range []error{nil, failure} {
			attempts := 0
			err := supervise(context.Background(), o, func() error {
				attempts++
				return result
			}, func(context.Context, time.Duration) bool {
				t.Fatal("bounded helper must never restart")
				return false
			})
			if !errors.Is(err, result) || attempts != 1 {
				t.Fatal(attempts, err)
			}
		}
	}
}

func TestSupervisorShutdownInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	waited := false
	err := supervise(ctx, launchOptions{}, func() error {
		attempts++
		return errors.New("stopped")
	}, func(ctx context.Context, delay time.Duration) bool {
		waited = true
		cancel() // Shutdown arrives between child exit and retry.
		return waitRetry(ctx, time.Hour)
	})
	if err != nil || attempts != 1 || !waited {
		t.Fatal("shutdown did not stop retries", attempts, err)
	}
	err = supervise(ctx, launchOptions{}, func() error {
		t.Fatal("started a helper after shutdown")
		return nil
	}, waitRetry)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorDoesNotWaitAfterStoppingChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := supervise(ctx, launchOptions{}, func() error {
		cancel()
		return context.Canceled
	}, func(context.Context, time.Duration) bool {
		t.Fatal("shutdown must not schedule another attempt")
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLauncherRejectsBroaderIdentityAndNetworkScope(t *testing.T) {
	base := launchOptions{binary: "/private/helper", config: "/private/helper.json", listen: "10.23.42.30:8742", device: "eth0", prefix: "10.23.42.0/24", peer: "10.23.42.17", uid: 1000, gid: 100, write: true, lifetime: time.Minute}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	subnet := base
	subnet.peer = "10.23.42.0/24"
	subnet.listen = ":8742"
	if err := subnet.validate(); err != nil {
		t.Fatal("direct client subnet and current-address listener rejected:", err)
	}
	for _, mode := range []string{"root", "root group", "wildcard", "hostname", "public", "outside subnet", "wider subnet", "noncanonical subnet", "low port", "current low port", "empty port", "unbounded", "long test", "relative"} {
		t.Run(mode, func(t *testing.T) {
			o := base
			switch mode {
			case "root":
				o.uid = 0
			case "root group":
				o.gid = 0
			case "wildcard":
				o.listen = "0.0.0.0:8742"
			case "hostname":
				o.listen = "nas:8742"
			case "public":
				o.listen = "8.8.8.8:8742"
			case "outside subnet":
				o.peer = "192.168.2.17"
			case "wider subnet":
				o.peer = "10.23.0.0/16"
			case "noncanonical subnet":
				o.peer = "10.23.42.17/24"
			case "current low port":
				o.listen = ":22"
			case "empty port":
				o.listen = ":"
			case "low port":
				o.listen = "10.23.42.30:22"
			case "unbounded":
				o.lifetime = 0
			case "long test":
				o.lifetime = time.Hour
			case "relative":
				o.binary = "helper"
			}
			if err := o.validate(); err == nil {
				t.Fatal("unsafe launcher scope accepted")
			}
		})
	}
}
