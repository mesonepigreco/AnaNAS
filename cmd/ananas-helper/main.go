// ananas-helper serves native NAS state under a dedicated unprivileged account.
// Live publication requires explicit configuration; --write-test is disposable-only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"nas-sync/internal/helper"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "anaNAS helper:", err)
		os.Exit(1)
	}
}

func run() (result error) {
	path := flag.String("config", "", "private native helper configuration file")
	check := flag.Bool("check-config", false, "validate local configuration, identity, TLS and native paths without creating a journal or opening a listener")
	fd := flag.Int("listen-fd", 3, "inherited IPv4 socket already bound to the configured device/address")
	writeTest := flag.Bool("write-test", false, "allow publication only under an explicitly prepared disposable helper test child")
	lifetime := flag.Duration("lifetime", 0, "optional lifetime; write tests require a positive value no greater than 5m")
	flag.Parse()
	if !*check {
		defer reportUsage()
	}
	if flag.NArg() != 0 || *path == "" || *lifetime < 0 || (*writeTest && (*lifetime <= 0 || *lifetime > 5*time.Minute)) {
		return fmt.Errorf("private config and valid bounded test lifetime required")
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return fmt.Errorf("run the helper as the dedicated non-root account")
	}
	c, err := helper.Load(*path)
	if err != nil {
		return err
	}
	if c, err = c.Resolved(); err != nil {
		return err
	}
	if err := helper.CheckFiles(c); err != nil {
		return err
	}
	if *writeTest {
		if err := c.CheckDisposable(); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *lifetime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *lifetime)
		defer cancel()
	}
	if err := helper.CheckNetwork(ctx, c); err != nil {
		return err
	}
	if *check {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Valid  bool `json:"valid"`
			Writes bool `json:"productionWrites"`
		}{Valid: true, Writes: c.LiveWrites})
	}
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(64 << 20)
	if err := unix.Setpriority(unix.PRIO_PROCESS, 0, 15); err != nil {
		return err
	}
	if *writeTest {
		if err := unix.Setrlimit(unix.RLIMIT_CPU, &unix.Rlimit{Cur: 30, Max: 45}); err != nil {
			return err
		}
	}
	listener, err := helper.InheritedListener(*fd, c)
	if err != nil {
		return err
	}
	defer listener.Close()
	service, err := helper.Open(c, *writeTest, func(ctx context.Context) error { return helper.CheckNetwork(ctx, c) })
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, service.Close()) }()
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Event      string `json:"event"`
		Listen     string `json:"listen"`
		TestWrites bool   `json:"testWrites"`
		LiveWrites bool   `json:"liveWrites"`
	}{Event: "serving", Listen: c.Listen, TestWrites: *writeTest, LiveWrites: c.LiveWrites}); err != nil {
		return err
	}
	return service.Serve(ctx, listener)
}

func reportUsage() {
	var u unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &u); err != nil {
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Event     string  `json:"event"`
		UID       int     `json:"uid"`
		GID       int     `json:"gid"`
		UserCPU   float64 `json:"userCPUSeconds"`
		SystemCPU float64 `json:"systemCPUSeconds"`
		MaxRSS    int64   `json:"maxRSSBytes"`
	}{Event: "stopped", UID: os.Geteuid(), GID: os.Getegid(), UserCPU: float64(u.Utime.Sec) + float64(u.Utime.Usec)/1e6, SystemCPU: float64(u.Stime.Sec) + float64(u.Stime.Usec)/1e6, MaxRSS: int64(u.Maxrss) * 1024})
}
