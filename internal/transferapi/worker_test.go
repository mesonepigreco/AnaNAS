package transferapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"nas-sync/internal/config"
	"nas-sync/internal/content"
	"nas-sync/internal/control"
	"nas-sync/internal/daemon"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/netwatch"
	"nas-sync/internal/observe"
	"nas-sync/internal/publish"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

func workerWait(t *testing.T, worker *replica.Worker, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("worker condition not reached", worker.Status())
}

// Exercise actual socket cancellation/reuse with a test-injected lifecycle event.
// This deliberately does not claim kernel route-fault injection.
type interruptedMonitor struct{ interrupt <-chan struct{} }

type countedObserver struct {
	daemon.Observer
	runs atomic.Int32
}

func (o *countedObserver) Run(ctx context.Context) error {
	o.runs.Add(1)
	return o.Observer.Run(ctx)
}

func (m interruptedMonitor) Watch(ctx context.Context, ready chan<- struct{}) error {
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (netwatch.Monitor{Interface: "lo"}).Watch(watchCtx, ready) }()
	select {
	case err := <-done:
		return err
	case <-m.interrupt:
		cancel()
		<-done
		return netwatch.ErrChanged
	case <-ctx.Done():
		cancel()
		<-done
		return ctx.Err()
	}
}

func TestTLSWorkerObservesUploadsPullsAndPauses(t *testing.T) {
	dir := t.TempDir()
	rootPath, state := filepath.Join(dir, "root"), filepath.Join(dir, "state")
	for _, p := range []string{rootPath, state} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := index.Open(filepath.Join(state, "index.db"), rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clientID, err := db.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	server := setupWithClientID(t, true, nil, clientID)
	client := newTestClient(t, clientOptions(server))
	c, err := journal.Open(state, identifier("worker replica"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 256 << 20, MaxEntries: 128}, server.c.Namespace()); err != nil {
		t.Fatal(err)
	}
	store, err := stage.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root, err := content.OpenRoot(rootPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publisher, err := publish.Open(rootPath, state, c.Version, nil, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	cfg := config.Default()
	cfg.Local.Root, cfg.StateDir, cfg.NAS.MountPoint = rootPath, state, "/mnt/unused-worker-test"
	cfg.Limits.ScanOpsPerSecond = 10000
	cfg.Coalesce.Idle, cfg.Coalesce.MaxWait = config.Duration(20*time.Millisecond), config.Duration(60*time.Millisecond)
	observer, err := observe.New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	controls, err := control.New(db)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := replica.NewWorker(db, root, c, store, publisher, client, replica.WorkerOptions{Namespace: server.c.Namespace(), PushOptions: pushOptions(), Ready: func() bool { s := observer.Status(); return s.Ready && !s.Scanning }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var allowed atomic.Bool
	allowed.Store(true)
	var networkPhase atomic.Value
	networkPhase.Store("")
	interrupt := make(chan struct{}, 1)
	observed := &countedObserver{Observer: observer}
	done := make(chan error, 1)
	go func() {
		done <- daemon.RunWithNetwork(ctx, observed, controls.Changes(), worker, daemon.Surface{Network: func(s daemon.NetworkStatus) { networkPhase.Store(s.Phase) }}, client, daemon.NetworkOptions{
			Monitor: interruptedMonitor{interrupt}, Check: func(ctx context.Context) error {
				if !allowed.Load() {
					return replica.ErrSuspended
				}
				return ctx.Err()
			},
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	workerWait(t, worker, func() bool { return observer.Status().Ready && worker.Status().Phase == "idle" })
	if err := worker.Run(ctx); err == nil {
		t.Fatal("second worker loop started")
	}
	if err := controls.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool { return worker.Status().Phase == "suspended" })
	base := make([]byte, 256*1024)
	if _, err := rand.Read(base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "file"), base, 0600); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		r, ok, err := db.Get("file")
		return err == nil && ok && r.Dirty && !observer.Status().Scanning
	})
	if _, err := os.Stat(filepath.Join(server.root, "file")); !os.IsNotExist(err) {
		t.Fatal("paused observer uploaded file", err)
	}
	if err := controls.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool { return worker.Status().CompletedUploads == 1 && worker.Status().Phase == "idle" })
	if b, err := os.ReadFile(filepath.Join(server.root, "file")); err != nil || !bytes.Equal(b, base) {
		t.Fatal("observer-driven upload differs", err)
	}
	// Parent and child arrive in one observer batch; the worker orders separate
	// proposals instead of creating an overlapping parent/child transaction.
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "folder/child"), []byte("nested"), 0600); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool { return worker.Status().CompletedUploads == 3 && worker.Status().Phase == "idle" })
	if b, err := os.ReadFile(filepath.Join(server.root, "folder/child")); err != nil || string(b) != "nested" {
		t.Fatal("nested upload failed", err)
	}
	before := client.Traffic()
	target := append([]byte{5}, base...)
	if err := os.WriteFile(filepath.Join(rootPath, "file"), target, 0600); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool { return worker.Status().CompletedUploads == 4 && worker.Status().Phase == "idle" })
	after := client.Traffic()
	if after.Sent <= before.Sent || after.Sent-before.Sent >= uint64(len(base)) {
		t.Fatal("automatic update did not save transport bytes", before, after)
	}
	if b, err := os.ReadFile(filepath.Join(server.root, "file")); err != nil || !bytes.Equal(b, target) {
		t.Fatal("automatic delta differs", err)
	}
	// A separate authenticated client commits a remote update. The persistent
	// authenticated stream must wake the worker without a test-supplied hint.
	remote := newTestClient(t, clientOptions(server))
	ack, err := db.Base("file")
	if err != nil || ack == nil {
		t.Fatal(ack, err)
	}
	remoteTarget := append([]byte{7}, target...)
	wire := encoded(t, target, remoteTarget)
	request, _ := server.request(t, "worker-remote-update", "file", ack.Content.ID, remoteTarget, wire)
	if _, err := remote.Apply(ctx, request, []io.Reader{bytes.NewReader(wire)}); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		dirty, err := db.DirtyPage("", 1)
		return err == nil && len(dirty) == 0 && worker.Status().AppliedRemoteBatches == 1 && worker.Status().Phase == "idle"
	})
	if b, err := os.ReadFile(filepath.Join(rootPath, "file")); err != nil || !bytes.Equal(b, remoteTarget) {
		t.Fatal("worker download differs", err)
	}
	if err := os.Remove(filepath.Join(rootPath, "file")); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		b, err := db.Base("file")
		return err == nil && b != nil && b.Content.Tombstone && worker.Status().Phase == "idle"
	})
	if _, err := os.Stat(filepath.Join(server.root, "file")); !os.IsNotExist(err) {
		t.Fatal("automatic deletion failed", err)
	}
	// No worker timer or refresh loop runs after all hints and durable work drain.
	remoteState, err := db.RemoteState()
	if err != nil || remoteState.Acknowledged != remoteState.Completed || remoteState.Completed == 0 {
		t.Fatal("worker did not acknowledge completed prefix", remoteState, err)
	}
	if cursor, err := server.c.Cursor(clientID); err != nil || cursor != remoteState.Completed {
		t.Fatal("NAS cursor differs from completed local work", cursor, remoteState, err)
	}
	history, err := c.Changes(0, journal.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range history {
		for _, entry := range record.Entries {
			if err := store.RequireWireVacant(entry.Next.ID); err != nil {
				t.Fatal("completed worker retained encoded spool", err)
			}
			if !entry.Next.Directory && !entry.Next.Tombstone {
				f, err := store.OpenReady(entry.Next.ID)
				if err != nil {
					t.Fatal("spool cleanup removed retained diff base", err)
				}
				f.Close()
			}
		}
	}
	u, err := c.PublicationBudgetUsage()
	if err != nil || u == nil || u.Entries != int64(worker.Status().CompletedUploads+worker.Status().AppliedRemoteBatches) || u.ReservedBytes <= 0 {
		t.Fatal("worker uploads and downloads were not charged", u, worker.Status(), err)
	}
	time.Sleep(1100 * time.Millisecond)
	idleTraffic, idleMetadata := client.Traffic(), observer.Status().MetadataReads
	time.Sleep(150 * time.Millisecond)
	if client.Traffic() != idleTraffic || observer.Status().MetadataReads != idleMetadata {
		t.Fatal("idle worker generated work", idleTraffic, client.Traffic())
	}
	// Disconnect the lifecycle, then make real local and remote edits while its
	// LAN check denies access. Observation must persist without a startup scan.
	scans := observer.Status().Scans
	allowed.Store(false)
	interrupt <- struct{}{}
	workerWait(t, worker, func() bool { return networkPhase.Load() == "offline" })
	if observer.Status().Scans != scans {
		t.Fatal("network interruption caused a scan", scans, observer.Status())
	}
	offlineTraffic := client.Traffic()
	if err := os.WriteFile(filepath.Join(rootPath, "offline-file"), []byte("local while offline"), 0600); err != nil {
		t.Fatal(err)
	}
	workerWait(t, worker, func() bool {
		r, ok, err := db.Get("offline-file")
		return err == nil && ok && r.Dirty && !observer.Status().Scanning
	})
	if _, err := os.Stat(filepath.Join(server.root, "offline-file")); !os.IsNotExist(err) {
		t.Fatal("offline upload", err)
	}
	childBase, err := db.Base("folder/child")
	if err != nil || childBase == nil {
		t.Fatal(childBase, err)
	}
	changedChild := []byte("nested edited remotely while PC offline")
	childWire := encoded(t, []byte("nested"), changedChild)
	childRequest, _ := server.request(t, "offline-remote-child", "folder/child", childBase.Content.ID, changedChild, childWire)
	if _, err := remote.Apply(ctx, childRequest, []io.Reader{bytes.NewReader(childWire)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if client.Traffic() != offlineTraffic {
		t.Fatal("offline client generated traffic", offlineTraffic, client.Traffic())
	}
	allowed.Store(true)
	interrupt <- struct{}{}
	workerWait(t, worker, func() bool {
		b, err := db.Base("offline-file")
		return err == nil && b != nil && worker.Status().Phase == "idle" && worker.Status().AppliedRemoteBatches == 2
	})
	if b, err := os.ReadFile(filepath.Join(server.root, "offline-file")); err != nil || string(b) != "local while offline" {
		t.Fatal("offline edit not uploaded", err)
	}
	if b, err := os.ReadFile(filepath.Join(rootPath, "folder/child")); err != nil || !bytes.Equal(b, changedChild) {
		t.Fatal("missed remote change not replayed", err)
	}
	if observed.runs.Load() != 1 {
		t.Fatal("network recovery restarted observation", observed.runs.Load())
	}
}
