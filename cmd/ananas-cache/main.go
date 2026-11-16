// ananas-cache inspects/reclaims superseded PC replica snapshots offline. Both
// database lifetime locks are required; it never contacts or modifies the NAS.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"nas-sync/internal/config"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/replica"
)

func main() {
	path := flag.String("config", config.DefaultPath(), "PC profile configuration")
	apply := flag.Bool("apply", false, "remove proven obsolete snapshots (default: report only)")
	details := flag.Bool("details", false, "emit each candidate as JSON before the summary")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *path, *apply, *details); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, path string, apply, details bool) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if !cfg.Sync.Enabled {
		return fmt.Errorf("configured PC replica required")
	}
	root, err := filepath.EvalSymlinks(cfg.Local.Root)
	if err != nil {
		return err
	}
	state := cfg.StateDir
	if state == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			base = filepath.Join(home, ".local", "state")
		}
		state = filepath.Join(base, "nas-sync", hash.SumBytes([]byte(root)).Hex()[:24])
	}
	if config.Overlap(state, root) || config.Overlap(state, cfg.NAS.MountPoint) {
		return fmt.Errorf("state overlaps sync root")
	}
	for _, name := range []string{"journal.db", "index.db"} {
		st, err := os.Lstat(filepath.Join(state, name))
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
			return fmt.Errorf("invalid existing private database: %s", name)
		}
	}
	c, err := journal.Open(state, cfg.Sync.ReplicaNamespace)
	if err != nil {
		return fmt.Errorf("stop the profile daemon before maintenance: %w", err)
	}
	defer c.Close()
	db, err := index.Open(filepath.Join(state, "index.db"), root)
	if err != nil {
		return err
	}
	defer db.Close()
	total := journal.CacheReport{}
	after := ""
	progress := time.Now()
	for {
		r, err := replica.MaintainCache(ctx, db, c, cfg.Sync.Namespace, after, 4096, apply)
		if err != nil {
			return err
		}
		if details {
			for _, item := range r.Details {
				if err := json.NewEncoder(os.Stdout).Encode(struct {
					Candidate journal.CacheCandidate `json:"candidate"`
				}{item}); err != nil {
					return err
				}
			}
		}
		total.Scanned += r.Scanned
		total.Candidates += r.Candidates
		total.Removed += r.Removed
		total.AllocatedBytes += r.AllocatedBytes
		total.LogicalBytes += r.LogicalBytes
		total.Missing += r.Missing
		if time.Since(progress) > time.Second {
			fmt.Fprintf(os.Stderr, "scanned=%d candidates=%d allocatedBytes=%d removed=%d\n", total.Scanned, total.Candidates, total.AllocatedBytes, total.Removed)
			progress = time.Now()
		}
		if r.Next == "" && r.Scanned > 0 || r.Next == "" && r.Removed == 0 {
			break
		}
		after = r.Next
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Apply bool `json:"apply"`
		journal.CacheReport
	}{apply, total})
}
