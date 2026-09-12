// Package cpugovernor applies a short per-task CPU burst policy to the current
// systemd cgroup. It changes only cpu.max and never moves processes.
package cpugovernor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const period int64 = 100000

type Governor struct {
	file    *os.File
	mu      sync.Mutex
	nextID  uint64
	closed  bool
	tasks   map[uint64]int64
	cancels map[uint64]chan struct{}
	percent atomic.Int64
	first   time.Duration
	second  time.Duration
}

func Open() (*Governor, error) {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var group string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "0::/") {
			group = strings.TrimPrefix(scanner.Text(), "0::/")
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if group == "" || filepath.Clean(group) != group || strings.HasPrefix(group, "../") {
		return nil, fmt.Errorf("unified service cgroup unavailable")
	}
	file, err := os.OpenFile(filepath.Join("/sys/fs/cgroup", group, "cpu.max"), os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open service CPU control: %w", err)
	}
	g := &Governor{file: file, first: 30 * time.Second, second: 30 * time.Second, tasks: make(map[uint64]int64), cancels: make(map[uint64]chan struct{})}
	if err := g.set(10); err != nil {
		file.Close()
		return nil, err
	}
	return g, nil
}

func (g *Governor) set(percent int64) error {
	if _, err := g.file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(g.file, "%d %d\n", percent*period/100, period); err != nil {
		return fmt.Errorf("set service CPU limit: %w", err)
	}
	g.percent.Store(percent)
	return nil
}

// Begin starts at 100%, drops to 50% after 30 seconds and 10% after another
// 30 seconds. The returned end function cannot throttle a newer task.
func (g *Governor) Begin() func() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return func() {}
	}
	g.nextID++
	id := g.nextID
	cancel := make(chan struct{})
	g.tasks[id] = 100
	g.cancels[id] = cancel
	_ = g.applyLocked()
	g.mu.Unlock()
	go g.decay(id, cancel)
	return func() { g.end(id) }
}

func (g *Governor) applyLocked() error {
	percent := int64(10)
	for _, taskPercent := range g.tasks {
		if taskPercent > percent {
			percent = taskPercent
		}
	}
	return g.set(percent)
}

func (g *Governor) decay(id uint64, cancel <-chan struct{}) {
	timer := time.NewTimer(g.first)
	defer timer.Stop()
	select {
	case <-cancel:
		return
	case <-timer.C:
	}
	g.mu.Lock()
	if g.closed || g.tasks[id] == 0 {
		g.mu.Unlock()
		return
	}
	g.tasks[id] = 50
	_ = g.applyLocked()
	g.mu.Unlock()
	timer.Reset(g.second)
	select {
	case <-cancel:
		return
	case <-timer.C:
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed && g.tasks[id] != 0 {
		g.tasks[id] = 10
		_ = g.applyLocked()
	}
}

func (g *Governor) end(id uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cancel := g.cancels[id]; cancel != nil {
		close(cancel)
		delete(g.cancels, id)
		delete(g.tasks, id)
		if !g.closed {
			_ = g.applyLocked()
		}
	}
}

func (g *Governor) Percent() int64 { return g.percent.Load() }

func (g *Governor) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	for id, cancel := range g.cancels {
		close(cancel)
		delete(g.cancels, id)
		delete(g.tasks, id)
	}
	_ = g.set(10)
	g.closed = true
	return g.file.Close()
}
