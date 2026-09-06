// Command nas-sync runs the LAN-only, diff-based folder synchronizer between a
// Linux machine and a QNAP NAS SMB/NFS share. See PLAN.md for the full design.
package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"flag"
	"fmt"
	"log"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/languard"
	"nas-sync/internal/observe"
	"nas-sync/internal/web"
	"os"
	"os/signal"
	"syscall"

	"nas-sync/internal/config"
)

var version = "0.0.0-dev"

func main() {
	cfgPath := flag.String("config", config.DefaultPath(), "path to the configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	printCfg := flag.Bool("print-config", false, "print the effective configuration and exit")
	scanOnce := flag.Bool("scan-once", false, "index local metadata once, print status, and exit (no NAS access)")
	checkLAN := flag.Bool("check-lan", false, "inspect local mount/route evidence without contacting the NAS")
	discoverNAS := flag.Bool("discover-nas", false, "list visible kernel and GVFS network mounts without contacting them")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *discoverNAS {
		mounts, err := languard.DiscoverSystem()
		if err != nil {
			log.Fatalf("discover NAS mounts: %v", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(mounts); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if *printCfg {
		if err := config.Print(os.Stdout, cfg); err != nil {
			log.Fatalf("print config: %v", err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("nas-sync %s (config: %s)", version, *cfgPath)
	log.Printf("local root: %s", cfg.Local.Root)
	log.Printf("nas mount:  %s (host=%s share=%s)", cfg.NAS.MountPoint, cfg.NAS.Host, cfg.NAS.Share)

	if *checkLAN {
		diagnosticCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := json.NewEncoder(os.Stdout).Encode(languard.Inspect(diagnosticCtx, cfg.NAS)); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := runObserver(ctx, cfg, *scanOnce); err != nil {
		log.Fatalf("nas-sync: %v", err)
	}
}

func runObserver(ctx context.Context, cfg *config.Config, once bool) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	root, err := filepath.EvalSymlinks(cfg.Local.Root)
	if err != nil {
		return fmt.Errorf("local root: %w", err)
	}
	// Reject aliased/overlapping roots before creating local state. Do not resolve
	// or stat the NAS mount: an automount might initiate network traffic.
	if config.Overlap(root, cfg.NAS.MountPoint) {
		return fmt.Errorf("resolved local root overlaps NAS mount")
	}
	effective := *cfg
	effective.Local.Root = root
	state := cfg.StateDir
	if state == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, e := os.UserHomeDir()
			if e != nil {
				return e
			}
			base = filepath.Join(home, ".local", "state")
		}
		if !filepath.IsAbs(base) {
			return fmt.Errorf("XDG_STATE_HOME must be absolute")
		}
		state = filepath.Join(base, "nas-sync", hash.SumBytes([]byte(root)).Hex()[:24])
	}
	if config.Overlap(state, root) || config.Overlap(state, cfg.NAS.MountPoint) {
		return fmt.Errorf("state directory overlaps a sync root")
	}
	// Existing symlinked ancestors must not redirect the state directory into a root.
	ancestor := state
	for {
		resolved, e := filepath.EvalSymlinks(ancestor)
		if e == nil {
			suffix, e := filepath.Rel(ancestor, state)
			if e != nil {
				return e
			}
			candidate := filepath.Join(resolved, suffix)
			if config.Overlap(candidate, root) || config.Overlap(candidate, cfg.NAS.MountPoint) {
				return fmt.Errorf("resolved state location overlaps a sync root")
			}
			break
		}
		if !os.IsNotExist(e) {
			return e
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return e
		}
		ancestor = parent
	}
	if err = os.MkdirAll(state, 0700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(state)
	if err != nil {
		return err
	}
	if config.Overlap(resolved, root) || config.Overlap(resolved, cfg.NAS.MountPoint) {
		return fmt.Errorf("resolved state directory overlaps a sync root")
	}
	db, err := index.Open(filepath.Join(resolved, "index.db"), root)
	if err != nil {
		return err
	}
	defer db.Close()
	observer, err := observe.New(&effective, db)
	if err != nil {
		return err
	}
	if once {
		if err := observer.ScanOnce(ctx); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(observer.Status())
	}
	log.Printf("observing local metadata; NAS synchronization is disabled pending capability validation")
	if cfg.WebPort == 0 {
		err = observer.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	statusServer, err := web.New(cfg.WebPort, func() any { return observer.Status() })
	if err != nil {
		return fmt.Errorf("start local status UI: %w", err)
	}
	statusServer.Start()
	log.Printf("local status UI: %s", statusServer.URL())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	observerErr := make(chan error, 1)
	go func() { observerErr <- observer.Run(runCtx) }()
	var runErr error
	observerDone := false
	select {
	case runErr = <-observerErr:
		observerDone = true
	case webErr := <-statusServer.Errors():
		runErr = fmt.Errorf("local status UI: %w", webErr)
	case <-ctx.Done():
		runErr = nil
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if shutdownErr := statusServer.Shutdown(shutdownCtx); shutdownErr != nil && runErr == nil && ctx.Err() == nil {
		runErr = fmt.Errorf("stop local status UI: %w", shutdownErr)
	}
	if !observerDone {
		// Ensure the observer has released its watcher and index before returning.
		runErr = <-observerErr
	}
	if ctx.Err() != nil {
		return nil
	}
	return runErr
}
