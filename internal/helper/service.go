package helper

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nas-sync/internal/daemon"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/nasobserve"
	"nas-sync/internal/observe"
	"nas-sync/internal/publish"
	"nas-sync/internal/stage"
	"nas-sync/internal/transferapi"
)

type Service struct {
	c            *journal.Coordinator
	store        *stage.Store
	publisher    *publish.Publisher
	api          *transferapi.Server
	tls          *tls.Config
	config       Config
	gate         func(context.Context) error
	mu           sync.Mutex
	running      bool
	used, closed bool
	closeErr     error
}

// Open assembles the native service but opens no listener and starts no worker.
// The mandatory gate comes from deployment; socket binding is not OS egress
// enforcement. Test writes are restricted to a private child of the existing
// disposable test parent. Live writes require explicit configuration for the
// user-authorized LAN trial; both modes retain the same native/TLS/policy checks.
func Open(c Config, writeTest bool, gate func(context.Context) error) (*Service, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := c.CheckIdentity(); err != nil {
		return nil, err
	}
	if gate == nil {
		return nil, fmt.Errorf("deployment gate required")
	}
	if writeTest && c.LiveWrites {
		return nil, fmt.Errorf("live and disposable write modes are mutually exclusive")
	}
	if writeTest {
		if err := c.CheckDisposable(); err != nil {
			return nil, err
		}
	}
	c.Peers = append([]string(nil), c.Peers...)
	c.Exclusions = append([]string(nil), c.Exclusions...)
	t, err := c.TLS()
	if err != nil {
		return nil, err
	}
	s, err := stage.Open(c.StateDir)
	if err != nil {
		return nil, err
	}
	var coordinator *journal.Coordinator
	p, err := publish.Open(c.Root, c.StateDir, func(id string) (*journal.Version, error) {
		if coordinator == nil {
			return nil, fmt.Errorf("journal not open")
		}
		return coordinator.Version(id)
	}, c.Exclusions, c.ReadBytesPerSecond)
	if err != nil {
		s.Close()
		return nil, err
	}
	coordinator, err = journal.Open(c.StateDir, c.Namespace)
	if err == nil {
		err = coordinator.ConfigurePublicationBudget(journal.PublicationBudget{MaxBytes: c.MaxCacheBytes, MaxEntries: c.MaxCacheEntries})
		if err != nil {
			coordinator.Close()
		}
	}
	if err != nil {
		p.Close()
		s.Close()
		return nil, err
	}
	api, err := transferapi.New(coordinator, s, p, transferapi.Options{Clients: c.Clients, Exclusions: c.Exclusions, Writes: writeTest || c.LiveWrites, DisposableTest: writeTest, MaxEventStreams: min(4, c.MaxConnections-1), MaxFileBytes: c.MaxFileBytes, MaxBatchBytes: c.MaxBatchBytes, ReadBytesPerSecond: c.ReadBytesPerSecond, Gate: gate})
	if err != nil {
		p.Close()
		coordinator.Close()
		s.Close()
		return nil, err
	}
	return &Service{c: coordinator, store: s, publisher: p, api: api, tls: t, config: c, gate: gate}, nil
}

func (c Config) CheckDisposable() error {
	work := filepath.Dir(c.Root)
	name := filepath.Base(work)
	prefix := "ananas-helper-test-"
	if c.Root != filepath.Join(work, "root") || c.StateDir != filepath.Join(work, "state") || filepath.Base(filepath.Dir(work)) != "nas-sync-capability-test" || !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+32 {
		return fmt.Errorf("write test requires a dedicated disposable helper child")
	}
	for _, b := range name[len(prefix):] {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return fmt.Errorf("invalid disposable child identity")
		}
	}
	// Opening through stage validates canonical components, native filesystem,
	// effective ownership and mode 0700 without enumerating the test parent.
	s, err := stage.Open(work)
	if err != nil {
		return err
	}
	return s.Close()
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return fmt.Errorf("stop helper service before closing native state")
	}
	if !s.closed {
		s.closed = true
		s.closeErr = errors.Join(s.publisher.Close(), s.c.Close(), s.store.Close())
	}
	return s.closeErr
}

// Serve consumes an already bound/validated TCP listener. It joins every active
// request before returning; cancellation cannot leave a publisher using closed
// database or staging handles. Runtime peer validation precedes application work.
func (s *Service) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil || listener.Addr().String() != s.config.Listen {
		return fmt.Errorf("configured listener required")
	}
	s.mu.Lock()
	if s.used || s.closed {
		s.mu.Unlock()
		return fmt.Errorf("helper service must be reopened before another run")
	}
	s.running, s.used = true, true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()
	if err := s.gate(ctx); err != nil {
		listener.Close()
		return err
	}
	limited, err := transferapi.LimitListener(listener, s.config.MaxConnections)
	if err != nil {
		listener.Close()
		return err
	}
	defer limited.Close()
	peers := make([]netip.Prefix, 0, len(s.config.Peers))
	for _, peer := range s.config.Peers {
		p, err := ParsePeer(peer)
		if err != nil {
			listener.Close()
			return err
		}
		peers = append(peers, p)
	}
	allowed := func(address netip.Addr) bool {
		for _, p := range peers {
			if p.Contains(address) {
				return true
			}
		}
		return false
	}
	server := transferapi.HTTPServer(s.api)
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !allowed(peer.Addr()) {
			http.Error(w, "peer not allowed", http.StatusForbidden)
			return
		}
		s.api.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, peer.Addr())))
	})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var nativeDone chan error
	if s.config.ObserverStateDir != "" {
		// Validate the private native directory before opening its metadata DB.
		private, e := stage.Open(s.config.ObserverStateDir)
		if e != nil {
			return e
		}
		defer private.Close()
		db, e := index.Open(filepath.Join(s.config.ObserverStateDir, "index.db"), s.config.Root)
		if e != nil {
			return e
		}
		defer db.Close()
		var o *observe.Observer
		w, e := nasobserve.New(db, s.c, s.publisher, s.store, s.config.MaxFileBytes, s.config.Exclusions, func() bool { v := o.Status(); return v.Ready && !v.Scanning })
		if e != nil {
			return e
		}
		o, e = observe.NewNative(s.config.Root, s.config.Exclusions, db)
		if e != nil {
			return e
		}
		nativeDone = make(chan error, 1)
		go func() { nativeDone <- daemon.Run(runCtx, o, nil, w, daemon.Surface{}) }()
	}
	server.BaseContext = func(net.Listener) context.Context { return runCtx }
	done := make(chan error, 1)
	go func() { done <- server.Serve(tls.NewListener(limited, s.tls)) }()
	select {
	case err = <-nativeDone:
		nativeDone = nil
		if err == nil && ctx.Err() == nil {
			err = fmt.Errorf("native observer stopped")
		}
		cancel()
		_ = server.Close()
		<-done
	case err = <-done:
		cancel()
		_ = server.Close()
	case <-ctx.Done():
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		closeErr := server.Shutdown(shutdown)
		stop()
		if closeErr != nil {
			_ = server.Close()
		}
		err = <-done
	}
	// Close alone does not join handlers. Track native operations explicitly by
	// the application barrier below before the caller can close its state.
	s.api.StopAndWait()
	if nativeDone != nil {
		<-nativeDone
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// CheckFiles validates private TLS material and pinned native root/state without
// creating a journal or opening a network listener.
func CheckFiles(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := c.CheckIdentity(); err != nil {
		return err
	}
	if _, err := c.TLS(); err != nil {
		return err
	}
	s, err := stage.Open(c.StateDir)
	if err != nil {
		return err
	}
	defer s.Close()
	p, err := publish.Open(c.Root, c.StateDir, func(string) (*journal.Version, error) { return nil, os.ErrNotExist }, c.Exclusions, c.ReadBytesPerSecond)
	if err != nil {
		return err
	}
	return p.Close()
}
