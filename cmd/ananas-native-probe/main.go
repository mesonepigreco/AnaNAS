// ananas-native-probe is an explicit disposable native-filesystem capability
// test. It is not a daemon, transport endpoint or production sync command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"golang.org/x/sys/unix"
	"nas-sync/internal/nativeprobe"
)

func main() {
	path := flag.String("test-dir", "", "existing canonical native nas-sync-capability-test directory")
	write := flag.Bool("write", false, "create and clean up a unique disposable child (estimated 8 MiB maximum fixtures)")
	expectUID := flag.Int("expect-uid", -1, "require this non-root real/effective UID before accessing the test directory")
	expectGID := flag.Int("expect-gid", -1, "require this non-root real/effective and supplementary GID")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	groups, err := os.Getgroups()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot inspect process identity:", err)
		os.Exit(1)
	}
	if *expectUID != -1 || *expectGID != -1 {
		valid := *expectUID > 0 && *expectGID > 0 && os.Getuid() == *expectUID && os.Geteuid() == *expectUID && os.Getgid() == *expectGID && os.Getegid() == *expectGID
		for _, group := range groups {
			valid = valid && group == *expectGID
		}
		if !valid {
			fmt.Fprintln(os.Stderr, "probe process does not have the required non-admin identity")
			os.Exit(1)
		}
	}
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(64 * 1024 * 1024)
	if err := unix.Setpriority(unix.PRIO_PROCESS, 0, 15); err != nil {
		fmt.Fprintln(os.Stderr, "cannot lower probe priority:", err)
		os.Exit(1)
	}
	if err := unix.Setrlimit(unix.RLIMIT_CPU, &unix.Rlimit{Cur: 15, Max: 20}); err != nil {
		fmt.Fprintln(os.Stderr, "cannot bound probe CPU:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	report, err := nativeprobe.Run(ctx, *path, *write)
	var usage unix.Rusage
	err = errors.Join(err, unix.Getrusage(unix.RUSAGE_SELF, &usage))
	result := struct {
		nativeprobe.Report
		UID              int     `json:"uid"`
		GID              int     `json:"gid"`
		Groups           []int   `json:"groups"`
		UserCPUSeconds   float64 `json:"userCPUSeconds"`
		SystemCPUSeconds float64 `json:"systemCPUSeconds"`
		MaxRSSBytes      int64   `json:"maxRSSBytes"`
		Error            string  `json:"error,omitempty"`
	}{Report: report, UID: os.Getuid(), GID: os.Getgid(), Groups: groups,
		UserCPUSeconds:   float64(usage.Utime.Sec) + float64(usage.Utime.Usec)/1e6,
		SystemCPUSeconds: float64(usage.Stime.Sec) + float64(usage.Stime.Usec)/1e6,
		MaxRSSBytes:      usage.Maxrss * 1024}
	if err != nil {
		result.Error = err.Error()
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
		fmt.Fprintln(os.Stderr, encodeErr)
		os.Exit(1)
	}
	if err != nil {
		os.Exit(1)
	}
}
