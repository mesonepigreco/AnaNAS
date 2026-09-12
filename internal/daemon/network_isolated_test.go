package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"nas-sync/internal/netwatch"
)

var isolatedHost = flag.String("ananas-host-netns", "", "explicit isolated network test: original host namespace identity")

type isolatedOutput struct{ data bytes.Buffer }

func (b *isolatedOutput) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > 16<<10 {
		return 0, fmt.Errorf("isolated command output exceeds 16 KiB")
	}
	return b.data.Write(p)
}

func isolatedIP(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "/usr/sbin/ip", args...)
	c.Env = []string{"PATH=/usr/sbin:/usr/bin", "LC_ALL=C"}
	var output isolatedOutput
	c.Stdout, c.Stderr = &output, &output
	err := c.Run()
	return output.data.Bytes(), err
}

// This gate reads only the tiny test namespace's kernel state. It deliberately
// accepts virtual Ethernet; it is not the production physical-LAN/mount gate.
func isolatedGate(ctx context.Context) error {
	dev, err := net.InterfaceByName("lan0")
	if err != nil {
		return err
	}
	if dev.Flags&net.FlagUp == 0 {
		return errors.New("fixture link down")
	}
	addresses, err := dev.Addrs()
	if err != nil {
		return err
	}
	found := false
	for _, address := range addresses {
		found = found || address.String() == "10.88.0.2/24"
	}
	if !found {
		return errors.New("fixture address absent")
	}
	raw, err := isolatedIP(ctx, "-j", "-4", "route", "get", "10.88.0.30", "from", "10.88.0.2")
	if err != nil {
		return fmt.Errorf("fixture route: %w", err)
	}
	var routes []struct{ Dev, Gateway, Type string }
	if err := json.Unmarshal(raw, &routes); err != nil {
		return err
	}
	if len(routes) != 1 || routes[0].Dev != "lan0" || routes[0].Gateway != "" || (routes[0].Type != "" && routes[0].Type != "unicast") {
		return errors.New("fixture direct route absent")
	}
	return ctx.Err()
}

// TestIsolatedKernelNetworkRecovery runs only by explicit invocation of a built
// test binary inside a fresh root-owned anonymous network namespace. It never
// enters another namespace, touches a mount/NAS or changes a host interface.
func TestIsolatedKernelNetworkRecovery(t *testing.T) {
	if *isolatedHost == "" {
		t.Skip("explicit fresh network namespace required")
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	initNS, err := os.Readlink("/proc/1/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 || !regexp.MustCompile(`^net:\[[0-9]+\]$`).MatchString(*isolatedHost) || current == *isolatedHost || current == initNS {
		t.Fatal("refusing mutations outside an isolated privileged network namespace")
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("fresh namespace with only loopback required", interfaces, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	ip := func(args ...string) {
		t.Helper()
		output, err := isolatedIP(ctx, args...)
		if err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, output)
		}
	}
	create := func() {
		ip("link", "add", "lan0", "type", "veth", "peer", "name", "peer0")
		ip("link", "set", "peer0", "up")
		ip("link", "set", "lan0", "up")
		ip("addr", "add", "10.88.0.2/24", "dev", "lan0")
	}
	create()
	if err := isolatedGate(ctx); err != nil {
		t.Fatal("initial kernel gate", err)
	}
	for _, tc := range []struct {
		name            string
		remove, restore func()
	}{
		{"route-loss", func() { ip("route", "del", "10.88.0.0/24", "dev", "lan0") }, func() { ip("route", "add", "10.88.0.0/24", "dev", "lan0", "src", "10.88.0.2") }},
		{"address-loss", func() { ip("addr", "del", "10.88.0.2/24", "dev", "lan0") }, func() { ip("addr", "add", "10.88.0.2/24", "dev", "lan0") }},
		{"link-down", func() { ip("link", "set", "lan0", "down") }, func() { ip("link", "set", "lan0", "up") }},
		{"interface-recreated", func() { ip("link", "del", "lan0") }, create},
		{"policy-rule", func() { ip("rule", "add", "priority", "100", "to", "10.88.0.30/32", "blackhole") }, func() { ip("rule", "del", "priority", "100") }},
		{"gateway-route", func() { ip("route", "add", "10.88.0.30/32", "via", "10.88.0.1", "dev", "lan0") }, func() { ip("route", "del", "10.88.0.30/32") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, w, r, _ := sessionFixtures()
			controls := make(chan struct{}, 1)
			offline := make(chan struct{}, 8)
			var checks atomic.Int32
			runCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() {
				done <- RunWithNetwork(runCtx, o, controls, w, Surface{Network: func(s NetworkStatus) {
					if s.Phase == "offline" {
						offline <- struct{}{}
					}
				}}, r, NetworkOptions{
					Monitor: netwatch.Monitor{Interface: "lan0"}, Check: func(ctx context.Context) error { checks.Add(1); return isolatedGate(ctx) },
				})
			}()
			defer sessionFinish(t, done, stop)
			await(t, o.started)
			await(t, w.started)
			await(t, r.started)
			await(t, w.notified)
			start := time.Now()
			tc.remove()
			await(t, w.stopped)
			await(t, r.stopped)
			stopElapsed := time.Since(start)
			sessionAwait(t, offline)
			if err := isolatedGate(ctx); err == nil {
				t.Fatal("fault did not deny local kernel gate")
			}
			before := checks.Load()
			// Observe longer than the minimum retry interval: denied policy must
			// sleep on events, while the local controller continues to work.
			sessionQuiet(t, w.started, 1100*time.Millisecond)
			if checks.Load() != before {
				t.Fatal("offline policy polled", before, checks.Load())
			}
			controls <- struct{}{}
			await(t, w.interrupted)
			sessionQuiet(t, o.stopped, time.Millisecond)
			start = time.Now()
			tc.restore()
			sessionAwait(t, w.started)
			sessionAwait(t, r.started)
			recoveryElapsed := time.Since(start)
			if err := isolatedGate(ctx); err != nil {
				t.Fatal("restored kernel gate denied", err)
			}
			sessionQuiet(t, o.stopped, time.Millisecond)
			report, _ := json.Marshal(map[string]any{"case": tc.name, "hostNamespace": *isolatedHost, "testNamespace": current, "commandStartToServicesStoppedMS": float64(stopElapsed.Microseconds()) / 1000, "restoreStartToServicesStartedMS": float64(recoveryElapsed.Microseconds()) / 1000, "gateChecks": checks.Load(), "passed": true})
			t.Log(string(report))
		})
		if t.Failed() {
			return
		}
	}
	ip("link", "del", "lan0")
	interfaces, err = net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("test interface cleanup failed", interfaces, err)
	}
}
