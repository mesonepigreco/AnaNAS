// Command nas-sync runs the LAN-only, diff-based folder synchronizer between a
// Linux machine and a QNAP NAS SMB/NFS share. See PLAN.md for the full design.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
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
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
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

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("nas-sync: %v", err)
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	// Engine wiring starts in M1. For now we validate that the configured
	// directories exist so early integration problems surface immediately.
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	<-ctx.Done()
	return nil
}
