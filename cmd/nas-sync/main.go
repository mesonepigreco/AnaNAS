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
	"nas-sync/internal/control"
	"nas-sync/internal/cpugovernor"
	"nas-sync/internal/daemon"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/languard"
	"nas-sync/internal/observe"
	"nas-sync/internal/traffic"
	"nas-sync/internal/web"
	"os"
	"os/signal"
	"syscall"

	"nas-sync/internal/config"
)

var version = "0.0.0-dev"

func cpuPercent(governor *cpugovernor.Governor) int64 {
	if governor == nil {
		return 0
	}
	return governor.Percent()
}

func main() {
	cfgPath := flag.String("config", config.DefaultPath(), "path to the configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	printCfg := flag.Bool("print-config", false, "print the effective configuration and exit")
	scanOnce := flag.Bool("scan-once", false, "index local metadata once, print status, and exit (no NAS access)")
	identity := flag.Bool("client-identity", false, "print the persistent local client ID for provisioning (daemon must be stopped; no NAS access)")
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

	log.Printf("anaNAS %s (config: %s)", version, *cfgPath)
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
	if err := runApplication(ctx, cfg, *scanOnce, *identity); err != nil {
		log.Fatalf("anaNAS: %v", err)
	}
}

func runObserver(ctx context.Context, cfg *config.Config, once bool) error {
	return runApplication(ctx, cfg, once, false)
}

func runApplication(ctx context.Context, cfg *config.Config, once, identity bool) error {
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
	if identity {
		id, err := db.ClientID()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"clientID": id})
	}
	controls, err := control.New(db)
	if err != nil {
		return fmt.Errorf("load persistent sync controls: %w", err)
	}
	observer, err := observe.New(&effective, db)
	if err != nil {
		return err
	}
	var governor *cpugovernor.Governor
	var task func() func()
	if os.Getenv("ANANAS_CPU_GOVERNOR") == "1" {
		governor, err = cpugovernor.Open()
		if err != nil {
			return fmt.Errorf("start CPU task governor: %w", err)
		}
		defer governor.Close()
		task = governor.Begin
		observer.SetTaskHook(task)
	}
	if once {
		if err := observer.ScanOnce(ctx); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(observer.Status())
	}
	trafficHistory, err := traffic.Open(filepath.Join(resolved, "traffic.db"))
	if err != nil {
		return fmt.Errorf("open traffic history: %w", err)
	}
	defer func() {
		if err := trafficHistory.Close(); err != nil {
			log.Printf("save traffic history: %v", err)
		}
	}()
	var live *syncRuntime
	if cfg.Sync.Enabled {
		live, err = openSync(&effective, resolved, db, observer, task, trafficHistory.Add)
		if err != nil {
			return fmt.Errorf("start LAN sync: %w", err)
		}
		defer live.Close()
		log.Printf("native LAN synchronization enabled for the live trial")
	} else {
		log.Printf("observing local metadata; NAS synchronization is disabled")
	}
	run := func(surface daemon.Surface) error {
		if live != nil {
			return live.run(ctx, observer, controls.Changes(), surface)
		}
		return daemon.Run(ctx, observer, controls.Changes(), nil, surface)
	}
	if cfg.WebPort == 0 {
		err = run(daemon.Surface{})
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	storage := &storageView{cfg: cfg.NAS, db: db}
	statusServer, err := web.NewWithPending(cfg.WebPort, func() any {
		status := observer.Status()
		reason := "NAS transfers disabled: synchronization engine and safety validation are incomplete."
		var uploaded, downloaded uint64
		activity, activityErr := db.Activity(time.Now())
		if live != nil {
			status.Mode = "native LAN synchronization — live trial"
			reason = live.reason(status)
			traffic := live.client.Traffic()
			uploaded, downloaded = traffic.Sent, traffic.Received
		}
		if activityErr != nil {
			reason = "Local activity history error: " + activityErr.Error()
		}
		return struct {
			observe.Status
			Paused          bool                 `json:"paused"`
			AutomaticWrites bool                 `json:"automaticWrites"`
			SyncReason      string               `json:"syncReason"`
			UploadBytes     uint64               `json:"uploadBytes"`
			DownloadBytes   uint64               `json:"downloadBytes"`
			Updated24Hours  uint64               `json:"updatedLast24Hours"`
			RecentUpdates   []index.RecentUpdate `json:"recentUpdates"`
			CPULimitPercent int64                `json:"cpuLimitPercent,omitempty"`
			NAS             config.NAS           `json:"nas"`
			Limits          config.Limits        `json:"limits"`
			Traffic         traffic.Summary      `json:"traffic"`
		}{Status: status, Paused: controls.Paused(), AutomaticWrites: live != nil, SyncReason: reason, UploadBytes: uploaded, DownloadBytes: downloaded, Updated24Hours: activity.UpdatedLast24Hours, RecentUpdates: activity.Recent, CPULimitPercent: cpuPercent(governor), NAS: cfg.NAS, Limits: cfg.Limits, Traffic: trafficHistory.Summary(time.Now())}
	}, controls.SetPaused, storage.snapshot, func(ctx context.Context, after string) (any, error) {
		return db.PendingSync(ctx, after, cfg.Sync.MaxFileBytes)
	}, func(ctx context.Context, request index.SyncConfirmation) (index.ConfirmationResult, error) {
		if live == nil {
			return index.ConfirmationResult{}, fmt.Errorf("synchronization is disabled")
		}
		result, err := db.ConfirmSync(ctx, request, cfg.Sync.MaxFileBytes)
		if err == nil {
			live.worker.RequestSync()
		}
		return result, err
	})
	if err != nil {
		return fmt.Errorf("start local status UI: %w", err)
	}
	statusServer.Start()
	log.Printf("local status UI: %s", statusServer.URL())

	runErr := run(daemon.Surface{Notify: statusServer.Notify, Errors: statusServer.Errors()})
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if shutdownErr := statusServer.Shutdown(shutdownCtx); shutdownErr != nil && runErr == nil && ctx.Err() == nil {
		runErr = fmt.Errorf("stop local status UI: %w", shutdownErr)
	}
	if ctx.Err() != nil {
		return nil
	}
	return runErr
}
