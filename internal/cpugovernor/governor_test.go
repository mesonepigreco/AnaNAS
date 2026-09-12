package cpugovernor

import (
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
