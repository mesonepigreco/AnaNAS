package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"runtime/debug"

	"nas-sync/internal/helper"
	"nas-sync/internal/helperprobe"
	"nas-sync/internal/transferapi"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "helper test:", err)
		os.Exit(1)
	}
}
func run() error {
	endpoint := flag.String("endpoint", "", "private helper IPv4 address:port")
	source := flag.String("source", "", "bound client IPv4")
	device := flag.String("interface", "", "bound physical interface")
	prefix := flag.String("prefix", "", "direct IPv4 subnet")
	namespace := flag.String("namespace", "", "disposable helper namespace")
	clientID := flag.String("client-id", "", "allowlisted client identity")
	pin := flag.String("server-pin", "", "server leaf SHA-256 fingerprint")
	certificate := flag.String("certificate", "", "private client certificate file")
	key := flag.String("key", "", "private client key file")
	ca := flag.String("ca", "", "private test CA certificate file")
	write := flag.Bool("write", false, "run fixed 512 KiB fixture only against an authenticated fresh disposable-test helper")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	destination, err := netip.ParseAddrPort(*endpoint)
	if err != nil {
		return err
	}
	local, err := netip.ParseAddr(*source)
	if err != nil {
		return err
	}
	identity, roots, err := helper.ReadIdentity(*certificate, *key, *ca)
	if err != nil {
		return err
	}
	network := helper.Config{Listen: netip.AddrPortFrom(local, destination.Port()).String(), Interface: *device, Prefix: *prefix, Peers: []string{destination.Addr().String()}}
	gate := func(ctx context.Context) error { return helper.CheckNetwork(ctx, network) }
	c, err := transferapi.NewClient(transferapi.ClientOptions{Namespace: *namespace, Endpoint: destination, Source: local, Interface: *device, Roots: roots, Certificate: identity, ServerFingerprint: *pin, ClientID: *clientID, Writes: *write, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Gate: gate})
	if err != nil {
		return err
	}
	defer c.Close()
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(64 << 20)
	report, err := helperprobe.Run(context.Background(), c, *clientID, *write)
	if encodeErr := json.NewEncoder(os.Stdout).Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}
