package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"

	"nas-sync/internal/config"
	"nas-sync/internal/content"
	"nas-sync/internal/daemon"
	"nas-sync/internal/helper"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/languard"
	"nas-sync/internal/netwatch"
	"nas-sync/internal/observe"
	"nas-sync/internal/publish"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
	"nas-sync/internal/transferapi"
)

type syncRuntime struct {
	worker    *replica.Worker
	client    *transferapi.Client
	journal   *journal.Coordinator
	store     *stage.Store
	root      *content.Root
	publisher *publish.Publisher
	network   daemon.NetworkOptions
	mu        sync.Mutex
	status    daemon.NetworkStatus
}

func openSync(cfg *config.Config, state string, db *index.DB, observer *observe.Observer, task func() func(), recordTraffic func(uint64, uint64)) (_ *syncRuntime, err error) {
	s := &syncRuntime{}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	identity, err := db.ClientID()
	if err != nil {
		return nil, err
	}
	certificate, ca, err := helper.ReadIdentity(cfg.Sync.Certificate, cfg.Sync.PrivateKey, cfg.Sync.CA)
	if err != nil {
		return nil, err
	}
	source := netip.MustParseAddr(cfg.Sync.Source)
	network := helper.Config{Listen: netip.AddrPortFrom(source, uint16(cfg.Sync.Port)).String(), Interface: cfg.NAS.Interface, Prefix: cfg.NAS.Prefix, Peers: []string{cfg.NAS.Host}}
	gate := func(ctx context.Context) error {
		if err := helper.CheckNetwork(ctx, network); err != nil {
			return err
		}
		// Read only kernel mount metadata, never the CIFS/GVFS root itself.
		raw, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			return err
		}
		mounts, err := languard.ParseMounts(raw)
		if err != nil {
			return err
		}
		if result := languard.EvaluateMount(cfg.NAS, mounts); !result.Eligible {
			return fmt.Errorf("NAS mount: %s", result.Reason)
		}
		return ctx.Err()
	}
	exclusions := append(append([]string{}, cfg.Selective.ExcludeLocal...), cfg.Selective.ExcludeRemote...)
	s.client, err = transferapi.NewClient(transferapi.ClientOptions{Namespace: cfg.Sync.Namespace, Endpoint: netip.AddrPortFrom(netip.MustParseAddr(cfg.NAS.Host), uint16(cfg.Sync.Port)), Source: source, Interface: cfg.NAS.Interface, Roots: ca, Certificate: certificate, ServerFingerprint: cfg.Sync.ServerFingerprint, ClientID: identity, Exclusions: exclusions, Writes: true, MaxFileBytes: cfg.Sync.MaxFileBytes, MaxBatchBytes: cfg.Sync.MaxBatchBytes, Gate: gate, RecordTraffic: recordTraffic})
	if err != nil {
		return nil, err
	}
	s.journal, err = journal.Open(state, cfg.Sync.ReplicaNamespace)
	if err != nil {
		return nil, err
	}
	if err = s.journal.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: cfg.Limits.CacheBytes, MaxEntries: cfg.Sync.MaxCacheEntries}, cfg.Sync.Namespace); err != nil {
		return nil, err
	}
	s.store, err = stage.Open(state)
	if err != nil {
		return nil, err
	}
	s.root, err = content.OpenRoot(cfg.Local.Root, exclusions)
	if err != nil {
		return nil, err
	}
	s.publisher, err = publish.Open(cfg.Local.Root, state, s.journal.Version, exclusions, cfg.Limits.ReadBytesPerSecond)
	if err != nil {
		return nil, err
	}
	s.worker, err = replica.NewWorker(db, s.root, s.journal, s.store, s.publisher, s.client, replica.WorkerOptions{Namespace: cfg.Sync.Namespace, PushOptions: replica.PushOptions{Writes: true, Exclusions: exclusions, MaxFileBytes: cfg.Sync.MaxFileBytes, MaxBatchBytes: cfg.Sync.MaxBatchBytes, ReadBytesPerSecond: cfg.Limits.ReadBytesPerSecond, Gate: gate}, Ready: func() bool { v := observer.Status(); return v.Ready && !v.Scanning }, Task: task})
	if err != nil {
		return nil, err
	}
	s.network = daemon.NetworkOptions{Monitor: netwatch.Monitor{Interface: cfg.NAS.Interface}, Check: gate}
	return s, nil
}

func (s *syncRuntime) Close() error {
	var err error
	if s.client != nil {
		err = errors.Join(err, s.client.Close())
	}
	if s.publisher != nil {
		err = errors.Join(err, s.publisher.Close())
	}
	if s.root != nil {
		err = errors.Join(err, s.root.Close())
	}
	if s.store != nil {
		err = errors.Join(err, s.store.Close())
	}
	if s.journal != nil {
		err = errors.Join(err, s.journal.Close())
	}
	return err
}

func (s *syncRuntime) run(ctx context.Context, o daemon.Observer, controls <-chan struct{}, surface daemon.Surface) error {
	surface.Network = func(status daemon.NetworkStatus) {
		s.mu.Lock()
		s.status = status
		s.mu.Unlock()
		if surface.Notify != nil {
			surface.Notify()
		}
	}
	return daemon.RunWithNetwork(ctx, o, controls, s.worker, surface, s.client, s.network)
}

func statusReason(network daemon.NetworkStatus, observation observe.Status, work replica.WorkerStatus) string {
	// Network policy is the outer authorization boundary, so its failures take
	// precedence over local progress. A healthy incremental scan is progress,
	// not an observer failure: the worker is deliberately quiescent until the
	// observer emits its completed-reconciliation hint.
	if network.LastError != "" {
		return network.Phase + ": " + network.LastError
	}
	if network.Phase != "active" {
		return "LAN sync: " + network.Phase
	}
	if observation.LastError != "" {
		return "Local observation error: " + observation.LastError
	}
	if !observation.Ready {
		return "Preparing local file index; synchronization will start automatically"
	}
	if observation.Scanning {
		return "Indexing local changes; synchronization will resume automatically"
	}
	if work.LastError != "" {
		if work.Phase == "retrying" {
			return "Temporary synchronization problem; retrying automatically: " + work.LastError
		}
		if work.Phase == "attention" {
			return "Synchronization needs attention: " + work.LastError
		}
		if work.Phase == "partial" {
			return "Eligible files synced; some paths need attention: " + work.LastError
		}
		return work.LastError
	}
	if work.Phase == "working" {
		return "Synchronizing"
	}
	return "LAN sync active"
}

func (s *syncRuntime) reason(observation observe.Status) string {
	s.mu.Lock()
	network := s.status
	s.mu.Unlock()
	return statusReason(network, observation, s.worker.Status())
}
