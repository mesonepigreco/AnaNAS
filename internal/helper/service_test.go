package helper

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/helperprobe"
	"nas-sync/internal/journal"
	"nas-sync/internal/socketpolicy"
	"nas-sync/internal/transferapi"
)

func boundListener(t *testing.T, c *Config) net.Listener {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var bindErr error
		err := raw.Control(func(fd uintptr) {
			bindErr = socketpolicy.BindIPv4(int(fd), "lo")
		})
		if err != nil {
			return err
		}
		return bindErr
	}}
	l, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.Listen = l.Addr().String()
	f, err := l.(*net.TCPListener).File()
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	l.Close()
	fd, err := unix.Dup(int(f.Fd()))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := InheritedListener(fd, *c)
	if err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	return listener
}

func TestInheritedListenerRefusesGatewayEnabledSocket(t *testing.T) {
	f := helperFixture(t)
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "lo"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	address, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Listen = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(address.(*unix.SockaddrInet4).Port)).String()
	if _, err := InheritedListener(fd, f.config); err == nil {
		t.Fatal("missing no-gateway flag accepted")
	}
	if _, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err != nil {
		t.Fatal("refused descriptor was closed", err)
	}
}

func TestNativeHelperServesInheritedSocketAndGatesWrites(t *testing.T) {
	for _, name := range []string{"read-only", "disposable-write", "live-write"} {
		write := name != "read-only"
		t.Run(name, func(t *testing.T) {
			f := helperFixture(t)
			f.config.LiveWrites = name == "live-write"
			if f.config.LiveWrites {
				f.config.ObserverStateDir = filepath.Join(filepath.Dir(f.config.StateDir), "observer")
				if err := os.Mkdir(f.config.ObserverStateDir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			listener := boundListener(t, &f.config)
			defer listener.Close()
			gate := func(ctx context.Context) error { return CheckNetwork(ctx, f.config) }
			if err := gate(context.Background()); err != nil {
				t.Fatal("source-bound route check", err)
			}
			s, err := Open(f.config, name == "disposable-write", gate)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.Serve(ctx, listener) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(6 * time.Second):
					t.Error("helper did not stop")
				}
			}()
			client, err := transferapi.NewClient(transferapi.ClientOptions{Namespace: f.config.Namespace, Endpoint: netip.MustParseAddrPort(f.config.Listen), Source: netip.MustParseAddr("127.0.0.1"), Interface: "lo", Roots: f.roots, Certificate: f.client, ServerFingerprint: f.pin, ClientID: strings.Repeat("b", 64), Writes: true, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Gate: func(context.Context) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			state, err := client.State(ctx)
			if err != nil || state.Writes != write {
				t.Fatal("helper state", state, err)
			}
			if name == "disposable-write" {
				report, err := helperprobe.Run(ctx, client, strings.Repeat("b", 64), true)
				if err != nil || len(report.Checks) != 10 || report.UploadLiteral != 1 || report.DownloadLiteral != 1 {
					t.Fatal(report, err)
				}
				usage, err := s.c.PublicationBudgetUsage()
				if err != nil || usage == nil || usage.Entries != 5 || usage.ReservedBytes != 4128771 {
					t.Fatal("native TLS operations did not retain their cache charges", usage, err)
				}
				return
			}
			if !write {
				report, err := helperprobe.Run(ctx, client, strings.Repeat("b", 64), false)
				if err != nil || len(report.Checks) != 3 || report.NotificationIdleSeconds < 2 || report.NotificationIdleTraffic != (transferapi.Traffic{}) {
					t.Fatal("read-only notification probe", report, err)
				}
			}
			data := []byte("native helper fixture")
			sig, err := delta.Build(ctx, bytes.NewReader(nil), 0, 65536)
			if err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			if _, err := delta.Encode(ctx, bytes.NewReader(data), int64(len(data)), sig, &wire); err != nil {
				t.Fatal(err)
			}
			v := journal.Version{ID: strings.Repeat("c", 64), Size: int64(len(data)), Digest: hash.SumBytes(data)}
			request := transferapi.ApplyRequest{Epoch: state.Epoch, Proposal: journal.Proposal{ID: strings.Repeat("d", 64), Client: strings.Repeat("b", 64), Entries: []journal.Entry{{Path: "file", Next: v}}}, DeltaBytes: []int64{int64(wire.Len())}}
			_, err = client.Apply(ctx, request, []io.Reader{&wire})
			if write {
				if err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(filepath.Join(f.config.Root, "file")); err != nil || !bytes.Equal(got, data) {
					t.Fatal("helper publication differs", err)
				}
				// An external native write must become an immutable, downloadable
				// version through actual inotify/coalescing, without a test hint.
				edited := []byte("edited directly on the NAS")
				if err := os.WriteFile(filepath.Join(f.config.Root, "file"), edited, 0600); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(8 * time.Second)
				var head *journal.Version
				for time.Now().Before(deadline) {
					h, pending, err := s.c.Head("file")
					if err != nil {
						t.Fatal(err)
					}
					if !pending && h != nil && h.Digest == hash.SumBytes(edited) {
						head = h
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if head == nil {
					t.Fatal("native edit was not imported")
				}
				signature, err := delta.Build(ctx, bytes.NewReader(nil), 0, 65536)
				if err != nil {
					t.Fatal(err)
				}
				var downloaded bytes.Buffer
				if _, err := client.DownloadVersion(ctx, "file", *head, signature, bytes.NewReader(nil), &downloaded); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(downloaded.Bytes(), edited) {
					t.Fatal("download did not retain native edit")
				}
				// Metadata-only feedback does one comparison, not another commit.
				seq, _, err := s.c.Observe()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(filepath.Join(f.config.Root, "file"), time.Now(), time.Now()); err != nil {
					t.Fatal(err)
				}
				time.Sleep(2 * time.Second)
				after, _, err := s.c.Observe()
				if err != nil || after != seq {
					t.Fatal("same content created a feedback version", seq, after, err)
				}
			} else {
				if err == nil {
					t.Fatal("read-only helper accepted publication")
				}
				if _, err := os.Stat(filepath.Join(f.config.Root, "file")); !os.IsNotExist(err) {
					t.Fatal("read-only helper changed visible root", err)
				}
			}
		})
	}
}

func TestHelperCancellationJoinsActiveApplicationRequest(t *testing.T) {
	f := helperFixture(t)
	listener := boundListener(t, &f.config)
	defer listener.Close()
	entered, returned := make(chan struct{}), make(chan struct{})
	gate := func(ctx context.Context) error {
		if ctx.Value(peerKey{}) != nil {
			close(entered)
			<-ctx.Done()
			close(returned)
			return ctx.Err()
		}
		return nil
	}
	s, err := Open(f.config, false, gate)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	client, err := transferapi.NewClient(transferapi.ClientOptions{Namespace: f.config.Namespace, Endpoint: netip.MustParseAddrPort(f.config.Listen), Source: netip.MustParseAddr("127.0.0.1"), Interface: "lo", Roots: f.roots, Certificate: f.client, ServerFingerprint: f.pin, ClientID: strings.Repeat("b", 64), MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Gate: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := make(chan error, 1)
	go func() { _, err := client.State(ctx); request <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("application request did not start")
	}
	if err := s.Close(); err == nil {
		t.Fatal("closed native state while serving")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("helper shutdown did not finish")
	}
	select {
	case <-returned:
	default:
		t.Fatal("service returned before its application request")
	}
	<-request
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := journal.Open(f.config.StateDir, f.config.Namespace)
	if err != nil {
		t.Fatal("shutdown retained journal lock", err)
	}
	reopened.Close()
}

func TestHelperShutdownJoinsQuietNotificationStream(t *testing.T) {
	f := helperFixture(t)
	listener := boundListener(t, &f.config)
	defer listener.Close()
	s, err := Open(f.config, false, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	client, err := transferapi.NewClient(transferapi.ClientOptions{Namespace: f.config.Namespace, Endpoint: netip.MustParseAddrPort(f.config.Listen), Source: netip.MustParseAddr("127.0.0.1"), Interface: "lo", Roots: f.roots, Certificate: f.client, ServerFingerprint: f.pin, ClientID: strings.Repeat("b", 64), MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Gate: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	hints, ended := make(chan struct{}, 1), make(chan error, 1)
	// The client has a separate context: server shutdown must end its stream.
	go func() { ended <- client.WatchChanges(context.Background(), hints) }()
	select {
	case <-hints:
	case <-time.After(5 * time.Second):
		t.Fatal("helper stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("quiet stream held helper shutdown")
	}
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("unexpected successful stream termination")
		}
	case <-time.After(time.Second):
		t.Fatal("client did not observe helper shutdown")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := journal.Open(f.config.StateDir, f.config.Namespace)
	if err != nil {
		t.Fatal("notification retained journal ownership", err)
	}
	reopened.Close()
}
