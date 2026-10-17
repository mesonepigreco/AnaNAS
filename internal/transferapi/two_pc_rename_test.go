package transferapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nas-sync/internal/config"
	"nas-sync/internal/content"
	"nas-sync/internal/daemon"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/observe"
	"nas-sync/internal/publish"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

type testPC struct {
	root, state, id string
	db              *index.DB
	worker          *replica.Worker
}

func newTestPC(t *testing.T) *testPC {
	t.Helper()
	dir := t.TempDir()
	p := &testPC{root: filepath.Join(dir, "root"), state: filepath.Join(dir, "state")}
	if err := os.Mkdir(p.root, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	p.db, err = index.Open(filepath.Join(p.state, "index.db"), p.root)
	if err != nil {
		t.Fatal(err)
	}
	p.id, err = p.db.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.db.Close() })
	return p
}
func (p *testPC) start(t *testing.T, server *fixture, options ClientOptions) {
	t.Helper()
	c, err := journal.Open(p.state, identifier(p.id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 256 << 20, MaxEntries: 512}, server.c.Namespace()); err != nil {
		t.Fatal(err)
	}
	store, err := stage.Open(p.state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root, err := content.OpenRoot(p.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	publisher, err := publish.Open(p.root, p.state, c.Version, nil, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publisher.Close() })
	cfg := config.Default()
	cfg.Local.Root, cfg.StateDir, cfg.NAS.MountPoint = p.root, p.state, "/mnt/unused"
	cfg.Coalesce.Idle, cfg.Coalesce.MaxWait = config.Duration(15*time.Millisecond), config.Duration(50*time.Millisecond)
	observer, err := observe.New(cfg, p.db)
	if err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, options)
	p.worker, err = replica.NewWorker(p.db, root, c, store, publisher, client, replica.WorkerOptions{Namespace: server.c.Namespace(), PushOptions: pushOptions(), Ready: func() bool { s := observer.Status(); return s.Ready && !s.Scanning }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.RunWithRemote(ctx, observer, nil, p.worker, daemon.Surface{}, client) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	workerWait(t, p.worker, func() bool { return observer.Status().Ready && p.worker.Status().Phase == "idle" })
}

func TestTwoPCDirectoryRenameAndConflictIsolation(t *testing.T) {
	a, b := newTestPC(t), newTestPC(t)
	server := setupWithClientID(t, true, nil, a.id)
	// Enroll a second independent certificate/identity before either PC connects.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(99), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	server.server.TLS.ClientCAs.AddCert(cert)
	pin := sha256.Sum256(der)
	server.api.opts.Clients[hex.EncodeToString(pin[:])] = b.id
	options := clientOptions(server)
	options.ClientID = b.id
	options.Certificate = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	a.start(t, server, clientOptions(server))
	b.start(t, server, options)
	t.Cleanup(func() {
		if t.Failed() {
			for _, pc := range []*testPC{a, b} {
				dirty, _ := pc.db.DirtyPage("", 30)
				problems, _ := pc.db.IncomingProblems(context.Background(), "")
				t.Log("PC", pc.id, pc.worker.Status(), "dirty", dirty, "problems", problems)
				for _, name := range []string{"old", "renamed", "aaa", "old/inner/file", "renamed/inner/file"} {
					r, _, _ := pc.db.Get(name)
					base, _ := pc.db.Base(name)
					_, err := os.Stat(filepath.Join(pc.root, name))
					t.Log(name, r, base, "stat", err)
				}
			}
		}
	})
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(a.root, "old/inner/empty"), 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(a.root, "old/inner/file"), "shared original")
	workerWait(t, b.worker, func() bool {
		data, err := os.ReadFile(filepath.Join(b.root, "old/inner/file"))
		_, dirErr := os.Stat(filepath.Join(b.root, "old/inner/empty"))
		return err == nil && string(data) == "shared original" && dirErr == nil && b.worker.Status().Phase == "idle"
	})
	for _, names := range [][2]string{{"old", "renamed"}, {"renamed", "aaa"}} {
		if err := os.Rename(filepath.Join(b.root, names[0]), filepath.Join(b.root, names[1])); err != nil {
			t.Fatal(err)
		}
		workerWait(t, a.worker, func() bool {
			data, err := os.ReadFile(filepath.Join(a.root, names[1], "inner/file"))
			_, oldErr := os.Stat(filepath.Join(a.root, names[0]))
			return err == nil && string(data) == "shared original" && os.IsNotExist(oldErr) && a.worker.Status().Phase == "idle"
		})
	}
	// A local edit on the receiving PC must survive a remote rename. Pause only
	// that PC while the other performs the rename through real TLS sync.
	if err := a.db.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	a.worker.Interrupt()
	workerWait(t, a.worker, func() bool { return a.worker.Status().Phase == "suspended" })
	write(filepath.Join(a.root, "aaa/inner/file"), "unsent local work")
	if err := os.Rename(filepath.Join(b.root, "aaa"), filepath.Join(b.root, "final")); err != nil {
		t.Fatal(err)
	}
	workerWait(t, b.worker, func() bool {
		_, err := os.Stat(filepath.Join(server.root, "aaa"))
		return os.IsNotExist(err) && b.worker.Status().Phase == "idle"
	})
	if err := a.db.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	a.worker.Notify()
	workerWait(t, a.worker, func() bool {
		problems, err := a.db.IncomingProblems(context.Background(), "")
		data, _ := os.ReadFile(filepath.Join(a.root, "final/inner/file"))
		return err == nil && problems.Total > 0 && string(data) == "shared original" && a.worker.Status().Phase == "partial"
	})
	write(filepath.Join(a.root, "unrelated"), "still uploading")
	workerWait(t, b.worker, func() bool {
		data, _ := os.ReadFile(filepath.Join(b.root, "unrelated"))
		return string(data) == "still uploading"
	})
	issue, err := a.db.Incoming("aaa")
	if err != nil || issue == nil {
		t.Fatal("missing tree-level decision", issue, err)
	}
	request := identifier("save rename conflict")
	if err := a.db.ChooseIncoming(index.IncomingChoice{Path: "aaa", Revision: issue.Revision, Choice: "nas"}, request); err != nil {
		t.Fatal(err)
	}
	a.worker.Notify()
	workerWait(t, a.worker, func() bool {
		problems, _ := a.db.IncomingProblems(context.Background(), "")
		_, err := os.Stat(filepath.Join(a.root, "aaa"))
		return problems.Total == 0 && os.IsNotExist(err) && a.worker.Status().Phase == "idle"
	})
	data, err := os.ReadFile(filepath.Join(a.state, "recovered", "aaa-"+request, "inner/file"))
	if err != nil || string(data) != "unsent local work" {
		t.Fatal("rename recovery lost local edit", string(data), err)
	}
}
