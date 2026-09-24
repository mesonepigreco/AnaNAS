package main

import (
	"context"
	"nas-sync/internal/config"
	"nas-sync/internal/index"
	"nas-sync/internal/languard"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type capacity struct {
	Available bool      `json:"available"`
	Total     uint64    `json:"total"`
	Used      uint64    `json:"used"`
	Free      uint64    `json:"free"`
	CheckedAt time.Time `json:"checkedAt"`
	Message   string    `json:"message,omitempty"`
}
type storageView struct {
	nas    func() config.NAS
	db     *index.DB
	mu     sync.Mutex
	cached capacity
}

func (s *storageView) snapshot(ctx context.Context, path string) (any, error) {
	usage, err := s.db.DirectoryUsage(ctx, path)
	if err != nil {
		return nil, err
	}
	return struct {
		Capacity capacity    `json:"capacity"`
		Usage    index.Usage `json:"usage"`
	}{s.space(ctx), usage}, nil
}
func (s *storageView) space(ctx context.Context) capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.cached.CheckedAt) < 30*time.Second {
		return s.cached
	}
	result := capacity{CheckedAt: time.Now(), Message: "NAS space is unavailable. Connect to the NAS network and refresh."}
	defer func() { s.cached = result }()
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return result
	}
	mounts, err := languard.ParseMounts(raw)
	nas := s.nas()
	if err != nil || !languard.EvaluateMount(nas, mounts).Eligible {
		return result
	}
	// Bound the subprocess and never stat an unmounted path (which would report
	// local disk capacity as NAS space). No recursive NAS traversal is needed.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/df", "-B1", "--output=size,used,avail", "--", nas.MountPoint)
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.WaitDelay = time.Second
	data, err := command.Output()
	if err != nil {
		return result
	}
	values := strings.Fields(string(data))
	if len(values) != 6 {
		return result
	}
	numbers := make([]uint64, 3)
	for i := range numbers {
		numbers[i], err = strconv.ParseUint(values[i+3], 10, 64)
		if err != nil {
			return result
		}
	}
	if numbers[1] > numbers[0] || numbers[2] > numbers[0] {
		result.Message = "NAS returned inconsistent space figures."
		return result
	}
	result.Available = true
	result.Total = numbers[0]
	result.Used = numbers[1]
	result.Free = numbers[2]
	result.Message = ""
	return result
}
