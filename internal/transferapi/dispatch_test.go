package transferapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

type failFirstPublication struct {
	p     journal.Publisher
	calls atomic.Int32
}

func (f *failFirstPublication) Publish(ctx context.Context, r journal.Record) error {
	if err := f.p.Publish(ctx, r); err != nil {
		return err
	}
	if f.calls.Add(1) == 1 {
		return errors.New("interrupted after visible publication")
	}
	return nil
}

type receiptOnlyRemote struct {
	*Client
	t        *testing.T
	recovers int
}

func (r *receiptOnlyRemote) CommitUpload(context.Context, string, uint64, journal.Proposal, []int64, []io.Reader) (journal.Record, error) {
	r.t.Fatal("recovery attempted to retransmit payload")
	return journal.Record{}, errors.New("unexpected retransmission")
}

func (r *receiptOnlyRemote) RecoverUpload(ctx context.Context, namespace string, epoch uint64, proposal journal.Proposal) (*journal.Record, error) {
	r.recovers++
	return r.Client.RecoverUpload(ctx, namespace, epoch, proposal)
}

func pushOptions() replica.PushOptions {
	return replica.PushOptions{Writes: true, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }}
}

// The local store deliberately contains no payload spool. Recovery must depend
// on the exact remote operation and durable outbox, even across index restart.
func TestTLSDispatchRecoversWithoutLocalPayload(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		name := "committed"
		if prepared {
			name = "prepared"
		}
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			if err := os.Chmod(state, 0700); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(state, "index.db")
			db, err := index.Open(filename, "/local")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { db.Close() }()
			clientID, err := db.ClientID()
			if err != nil {
				t.Fatal(err)
			}
			f := setupWithClientID(t, true, nil, clientID)
			if prepared {
				f.api.publisher = &failFirstPublication{p: f.publisher}
			}
			client := newTestClient(t, clientOptions(f))
			data := []byte("retained remote candidate")
			wire := encoded(t, nil, data)
			request, _ := f.request(t, name, "file", "", data, wire)
			if err := db.Put([]index.Record{{Path: "file", Fingerprint: index.Fingerprint{Size: int64(len(data))}}}, false); err != nil {
				t.Fatal(err)
			}
			u := index.Upload{Namespace: f.c.Namespace(), Proposal: request.Proposal, Generations: []uint64{1}, DeltaBytes: request.DeltaBytes, DeltaDigests: []hash.Digest{hash.SumBytes(wire)}}
			if err := db.PrepareUpload(u); err != nil {
				t.Fatal(err)
			}
			_, err = client.Apply(context.Background(), request, []io.Reader{bytes.NewReader(wire)})
			if (err != nil) != prepared {
				t.Fatal("unexpected initial publication outcome", err)
			}
			// Omit the response receipt and reopen the index.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = index.Open(filename, "/local")
			if err != nil {
				t.Fatal(err)
			}
			store, err := stage.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			remote := &receiptOnlyRemote{Client: client, t: t}
			pusher, err := replica.NewPusher(db, store, remote, pushOptions())
			if err != nil {
				t.Fatal(err)
			}
			record, err := pusher.Dispatch(context.Background())
			if err != nil || record == nil || !record.Committed || record.ID != u.Proposal.ID {
				t.Fatal(record, err)
			}
			if (remote.recovers == 1) != prepared {
				t.Fatal("unexpected recovery requests", remote.recovers)
			}
			pending, err := db.PendingUpload()
			if err != nil || pending == nil || pending.Receipt == nil {
				t.Fatal(pending, err)
			}
			if err := db.CompleteUpload(record.ID); !errors.Is(err, index.ErrStale) {
				t.Fatal("receipt cleared unapplied bases", err)
			}
			actual, err := os.ReadFile(filepath.Join(f.root, "file"))
			if err != nil || !bytes.Equal(actual, data) {
				t.Fatal("recovered content differs", err)
			}
			if changes, err := f.c.Changes(0, 2); err != nil || len(changes) != 1 {
				t.Fatal("duplicate commit", changes, err)
			}
			traffic := client.Traffic()
			if _, err := pusher.Dispatch(context.Background()); err != nil || client.Traffic() != traffic {
				t.Fatal("saved receipt caused traffic", err)
			}
		})
	}
}

func TestTLSDispatchPolicyStopsBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"disabled", "paused", "excluded", "observed-excluded", "size", "gate", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			state := t.TempDir()
			if err := os.Chmod(state, 0700); err != nil {
				t.Fatal(err)
			}
			db, err := index.Open(filepath.Join(state, "index.db"), "/local")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			id, err := db.ClientID()
			if err != nil {
				t.Fatal(err)
			}
			f := setupWithClientID(t, true, nil, id)
			client := newTestClient(t, clientOptions(f))
			data := []byte("test")
			wire := encoded(t, nil, data)
			request, _ := f.request(t, mode, "file", "", data, wire)
			if err := db.Put([]index.Record{{Path: "file", Fingerprint: index.Fingerprint{Size: 4}}}, false); err != nil {
				t.Fatal(err)
			}
			if err := db.PrepareUpload(index.Upload{Namespace: f.c.Namespace(), Proposal: request.Proposal, Generations: []uint64{1}, DeltaBytes: request.DeltaBytes, DeltaDigests: []hash.Digest{hash.SumBytes(wire)}}); err != nil {
				t.Fatal(err)
			}
			store, err := stage.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			o := pushOptions()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "disabled":
				o.Writes = false
			case "paused":
				err = db.SetPaused(true)
			case "excluded":
				o.Exclusions = []string{"file"}
			case "observed-excluded":
				err = db.Put([]index.Record{{Path: "file", Fingerprint: index.Fingerprint{Size: 4}, Excluded: true}}, false)
			case "size":
				o.MaxFileBytes = 1
			case "gate":
				o.Gate = func(context.Context) error { return errors.New("LAN lost") }
			case "canceled":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			pusher, err := replica.NewPusher(db, store, client, o)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pusher.Dispatch(ctx); err == nil {
				t.Fatal("policy accepted dispatch")
			}
			if client.Traffic() != (Traffic{}) {
				t.Fatal("rejected dispatch generated traffic")
			}
			pending, err := db.PendingUpload()
			if err != nil || pending == nil || pending.Receipt != nil {
				t.Fatal("rejected dispatch acknowledged work", pending, err)
			}
		})
	}
}

func TestTLSNamespaceRequiredBeforeOperations(t *testing.T) {
	f := setup(t, true, nil)
	o := clientOptions(f)
	o.Namespace = ""
	if c, err := NewClient(o); err == nil {
		c.Close()
		t.Fatal("unbound client accepted")
	}
	for _, namespace := range []string{"", identifier("different namespace")} {
		req, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/state", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Ananas-Namespace", namespace)
		response, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusConflict {
			t.Fatal("wrong namespace accepted", response.Status)
		}
	}
	o = clientOptions(f)
	o.Namespace = identifier("different namespace")
	client := newTestClient(t, o)
	wire := encoded(t, nil, []byte("test"))
	request, _ := f.request(t, "wrong-target", "file", "", []byte("test"), wire)
	if _, err := client.Apply(context.Background(), request, []io.Reader{bytes.NewReader(wire)}); err == nil {
		t.Fatal("cross-namespace upload accepted")
	}
	if _, err := os.Stat(filepath.Join(f.root, "file")); !os.IsNotExist(err) {
		t.Fatal("cross-namespace upload published", err)
	}
	if record, err := f.c.Operation(request.Proposal.ID); err != nil || record != nil {
		t.Fatal("cross-namespace operation recorded", record, err)
	}
}
