package cpugovernor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testGovernor(t *testing.T) *Governor {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(t.TempDir(), "cpu.max"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	g := &Governor{file: file, first: 10 * time.Millisecond, second: 10 * time.Millisecond, tasks: make(map[uint64]int64), cancels: make(map[uint64]chan struct{})}
	if err := g.set(10); err != nil {
		t.Fatal(err)
	}
	return g
}

func waitPercent(t *testing.T, g *Governor, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for g.Percent() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if g.Percent() != want {
		t.Fatalf("CPU tier = %d, want %d", g.Percent(), want)
	}
}

func TestTaskBurstDecaysAndReturnsToIdle(t *testing.T) {
	g := testGovernor(t)
	end := g.Begin()
	waitPercent(t, g, 100)
	waitPercent(t, g, 50)
	waitPercent(t, g, 10)
	end()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedTaskCancelsPendingDecay(t *testing.T) {
	g := testGovernor(t)
	end := g.Begin()
	end()
	waitPercent(t, g, 10)
	time.Sleep(30 * time.Millisecond)
	if g.Percent() != 10 {
		t.Fatalf("completed task changed tier to %d", g.Percent())
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOverlappingTaskRestoresOlderTier(t *testing.T) {
	g := testGovernor(t)
	endLong := g.Begin()
	waitPercent(t, g, 50)
	endShort := g.Begin()
	waitPercent(t, g, 100)
	endShort()
	waitPercent(t, g, 50)
	endLong()
	waitPercent(t, g, 10)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReportsAnUndelegatedCPUControllerAsUnavailable(t *testing.T) {
	root := t.TempDir()
	membership := filepath.Join(root, "cgroup")
	if err := os.WriteFile(membership, []byte("0::/user.slice/app.slice/ananas.service\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Distributions before systemd 252 delegate only memory and pids, so the
	// service cgroup exists with no cpu.max in it.
	service := filepath.Join(root, "user.slice/app.slice/ananas.service")
	if err := os.MkdirAll(service, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := open(membership, root); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("undelegated cpu controller: err = %v, want ErrUnavailable", err)
	}
	if err := os.WriteFile(filepath.Join(service, "cpu.max"), []byte("max 100000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err := open(membership, root)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.Percent() != 10 {
		t.Fatalf("idle tier = %d, want 10", g.Percent())
	}
}

func TestOpenReportsAMissingUnifiedCgroupAsUnavailable(t *testing.T) {
	root := t.TempDir()
	membership := filepath.Join(root, "cgroup")
	if err := os.WriteFile(membership, []byte("1:name=systemd:/legacy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := open(membership, root); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cgroup v1 host: err = %v, want ErrUnavailable", err)
	}
}
