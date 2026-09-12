// ananas-helper-launch performs only privileged socket binding and starts the
// service with a non-root credential. It never reads configuration/TLS contents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nas-sync/internal/helper"
	"nas-sync/internal/socketpolicy"
)

type launchOptions struct {
	binary, config, listen, device, prefix, peer string
	uid, gid                                     int
	write                                        bool
	lifetime                                     time.Duration
}

func (o launchOptions) validate() error {
	for _, path := range []string{o.binary, o.config} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return fmt.Errorf("canonical helper and config paths required")
		}
	}
	endpoint, err := netip.ParseAddrPort(o.listen)
	if err != nil || !endpoint.Addr().Is4() || endpoint.Port() < 1024 || (!endpoint.Addr().IsPrivate() && !endpoint.Addr().IsLoopback()) {
		return fmt.Errorf("specific private IPv4 listener on an unprivileged port required")
	}
	prefix, err := netip.ParsePrefix(o.prefix)
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || !prefix.Contains(endpoint.Addr()) {
		return fmt.Errorf("matching IPv4 prefix required")
	}
	peer, err := netip.ParseAddr(o.peer)
	if err != nil || !peer.Is4() || !prefix.Contains(peer) || peer.IsLoopback() != endpoint.Addr().IsLoopback() || (!peer.IsPrivate() && !peer.IsLoopback()) {
		return fmt.Errorf("explicit same-subnet peer required")
	}
	if o.device == "" || len(o.device) > 15 || strings.ContainsAny(o.device, "/\\\x00 \t\r\n") || (endpoint.Addr().IsLoopback() != (o.device == "lo")) {
		return fmt.Errorf("explicit matching interface required")
	}
	if o.uid <= 0 || o.gid <= 0 || uint64(o.uid) > uint64(^uint32(0))-1 || uint64(o.gid) > uint64(^uint32(0))-1 {
		return fmt.Errorf("non-root runtime identity required")
	}
	if o.lifetime < 0 || (o.write && (o.lifetime <= 0 || o.lifetime > 5*time.Minute)) {
		return fmt.Errorf("write test requires a bounded lifetime up to 5m")
	}
	return nil
}

func main() {
	var o launchOptions
	flag.StringVar(&o.binary, "helper", "", "canonical helper executable")
	flag.StringVar(&o.config, "config", "", "private helper configuration (read only by the unprivileged child)")
	flag.StringVar(&o.listen, "listen", "", "specific private IPv4 address:port")
	flag.StringVar(&o.device, "interface", "", "physical device (lo only for local tests)")
	flag.StringVar(&o.prefix, "prefix", "", "direct IPv4 subnet")
	flag.StringVar(&o.peer, "peer", "", "explicit directly connected client IPv4")
	flag.IntVar(&o.uid, "uid", -1, "dedicated helper UID")
	flag.IntVar(&o.gid, "gid", -1, "dedicated helper GID and sole supplementary group")
	flag.BoolVar(&o.write, "write-test", false, "disposable-only write test")
	flag.DurationVar(&o.lifetime, "lifetime", 0, "helper lifetime; required for write tests")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "anaNAS launcher:", err)
		os.Exit(1)
	}
}

func run(o launchOptions) error {
	if err := o.validate(); err != nil {
		return err
	}
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return fmt.Errorf("launcher needs privilege only to bind the device and set the child identity")
	}
	st, err := os.Lstat(o.binary)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || st.Mode().Perm()&0022 != 0 || st.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("helper must be a regular executable without set-ID or group/world write bits")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	network := helper.Config{Listen: o.listen, Interface: o.device, Prefix: o.prefix, Peers: []string{o.peer}}
	if err := helper.CheckNetwork(ctx, network); err != nil {
		return err
	}
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var bindErr error
		err := raw.Control(func(fd uintptr) {
			bindErr = socketpolicy.BindIPv4(int(fd), o.device)
		})
		return errors.Join(err, bindErr)
	}}
	listener, err := lc.Listen(ctx, "tcp4", o.listen)
	if err != nil {
		return err
	}
	f, err := listener.(*net.TCPListener).File()
	listener.Close()
	if err != nil {
		return err
	}
	defer f.Close()
	args := []string{"-config", o.config, "-listen-fd", "3"}
	if o.write {
		args = append(args, "-write-test")
	}
	if o.lifetime > 0 {
		args = append(args, "-lifetime", o.lifetime.String())
	}
	child := exec.Command(o.binary, args...)
	child.ExtraFiles = []*os.File{f}
	child.Stdin = nil
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	child.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(o.uid), Gid: uint32(o.gid), Groups: []uint32{uint32(o.gid)}}, Pdeathsig: syscall.SIGTERM}
	if err := child.Start(); err != nil {
		return err
	}
	f.Close()
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = child.Process.Signal(syscall.SIGTERM)
		return <-done
	}
}
