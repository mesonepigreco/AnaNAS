package transferapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
)

func clientOptions(f *fixture) ClientOptions {
	tlsConfig := f.client.Transport.(*http.Transport).TLSClientConfig
	pin := sha256.Sum256(f.server.TLS.Certificates[0].Certificate[0])
	return ClientOptions{Namespace: f.c.Namespace(), Endpoint: netip.MustParseAddrPort(strings.TrimPrefix(f.server.URL, "https://")), Source: netip.MustParseAddr("127.0.0.1"), Interface: "lo", Roots: tlsConfig.RootCAs, Certificate: tlsConfig.Certificates[0], ServerFingerprint: hex.EncodeToString(pin[:]), ClientID: f.clientID, Writes: true, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Gate: func(context.Context) error { return nil }}
}

func newTestClient(t *testing.T, o ClientOptions) *Client {
	t.Helper()
	c, err := NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestClientTLSRoundTrip(t *testing.T) {
	f := setup(t, true, nil)
	c := newTestClient(t, clientOptions(f))
	ctx := context.Background()
	state, err := c.State(ctx)
	if err != nil || state.Epoch != f.c.Epoch() || !state.Writes {
		t.Fatal(state, err)
	}
	base := make([]byte, 256*1024)
	if _, err := rand.Read(base); err != nil {
		t.Fatal(err)
	}
	wire := encoded(t, nil, base)
	first, _ := f.request(t, "client-create", "file", "", base, wire)
	result, err := c.Apply(ctx, first, []io.Reader{bytes.NewReader(wire)})
	if err != nil || !result.Record.Committed {
		t.Fatal(result, err)
	}
	head, err := c.Head(ctx, "file")
	if err != nil || head.Version == nil || head.Version.ID != first.Proposal.Entries[0].Next.ID {
		t.Fatal(head, err)
	}
	sig, err := c.Signature(ctx, Selection{"file", head.Version.ID}, *head.Version)
	if err != nil {
		t.Fatal(err)
	}
	target := append([]byte{17}, base...)
	var stream bytes.Buffer
	stats, err := delta.Encode(ctx, bytes.NewReader(target), int64(len(target)), sig, &stream)
	if err != nil || stats.LiteralBytes != 1 {
		t.Fatal(stats, err)
	}
	next, _ := f.request(t, "client-edit", "file", head.Version.ID, target, stream.Bytes())
	result, err = c.Apply(ctx, next, []io.Reader{bytes.NewReader(stream.Bytes())})
	if err != nil || len(result.Transfers) != 1 || result.Transfers[0].LiteralBytes != 1 {
		t.Fatal(result, err)
	}
	retry, err := c.Apply(ctx, next, []io.Reader{bytes.NewReader(stream.Bytes())})
	if err != nil || retry.Record.Sequence != result.Record.Sequence {
		t.Fatal(retry, err)
	}
	want := next.Proposal.Entries[0].Next
	var out bytes.Buffer
	stats, err = c.Download(ctx, Selection{"file", want.ID}, want, sig, bytes.NewReader(base), &out)
	if err != nil || stats.LiteralBytes != 1 || !bytes.Equal(out.Bytes(), target) {
		t.Fatal(stats, err)
	}
	visible, err := os.ReadFile(filepath.Join(f.root, "file"))
	if err != nil || !bytes.Equal(visible, target) {
		t.Fatal("published bytes differ", err)
	}
	traffic := c.Traffic()
	if traffic.Sent == 0 || traffic.Received == 0 || len(c.changes) != 1 {
		t.Fatal(traffic, len(c.changes))
	}
	for range 1000 {
		c.notify()
	}
	if len(c.changes) != 1 {
		t.Fatal("unbounded notifications")
	}
}

func TestClientFollowsCurrentSourceAddress(t *testing.T) {
	f := setup(t, true, nil)
	o := clientOptions(f)
	if _, err := NewClient(func() ClientOptions {
		v := o
		v.SourceFunc = func() (netip.Addr, error) { return v.Source, nil }
		return v
	}()); err == nil {
		t.Fatal("fixed and current source accepted together")
	}
	calls := 0
	o.Source = netip.Addr{}
	o.SourceFunc = func() (netip.Addr, error) { calls++; return netip.MustParseAddr("127.0.0.1"), nil }
	c := newTestClient(t, o)
	if _, err := c.State(context.Background()); err != nil || calls == 0 {
		t.Fatal(calls, err)
	}
	o.SourceFunc = func() (netip.Addr, error) { return netip.Addr{}, fmt.Errorf("no address") }
	c = newTestClient(t, o)
	if _, err := c.State(context.Background()); err == nil {
		t.Fatal("missing current source accepted")
	}
}

func TestClientRejectsBeforeNetwork(t *testing.T) {
	f := setup(t, true, nil)
	o := clientOptions(f)
	var calls atomic.Int32
	o.Gate = func(context.Context) error { calls.Add(1); return errors.New("LAN unavailable") }
	o.Exclusions = []string{"secret/"}
	c := newTestClient(t, o)
	if _, err := c.Head(context.Background(), "secret/file"); err == nil || calls.Load() != 0 {
		t.Fatal("exclusion evaluated too late", err)
	}
	if _, err := c.State(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatal("closed gate ignored", err)
	}
	if c.Traffic() != (Traffic{}) {
		t.Fatal("closed gate caused traffic")
	}
	c.Close()
	if _, err := c.State(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatal("closed client started work", err)
	}
	for _, endpoint := range []string{"8.8.8.8:443", "[::1]:443", "127.0.0.1:0"} {
		o.Endpoint = netip.MustParseAddrPort(endpoint)
		if _, err := NewClient(o); err == nil {
			t.Fatal("accepted endpoint", endpoint)
		}
	}
}

func TestClientRejectsChangedServerPin(t *testing.T) {
	f := setup(t, true, nil)
	o := clientOptions(f)
	o.ServerFingerprint = identifier("different server")
	c := newTestClient(t, o)
	if _, err := c.State(context.Background()); err == nil || !strings.Contains(err.Error(), "pin changed") {
		t.Fatal(err)
	}
}

// The replacement peer uses the same valid identity. Protocol validation must
// still reject a buggy authenticated server's response before publication.
func clientWithPeer(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	f := setup(t, true, nil)
	s := httptest.NewUnstartedServer(handler)
	s.TLS = f.server.TLS.Clone()
	s.StartTLS()
	t.Cleanup(s.Close)
	o := clientOptions(f)
	o.Endpoint = netip.MustParseAddrPort(strings.TrimPrefix(s.URL, "https://"))
	return newTestClient(t, o)
}

func TestClientRejectsBadMetadataAndRedirect(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType string
		status                  int
	}{
		{"oversize", strings.Repeat(" ", 1025), "application/json", 200},
		{"unknown", `{"epoch":1,"writes":true,"extra":1}`, "application/json", 200},
		{"trailing", `{"epoch":1,"writes":true}{}`, "application/json", 200},
		{"wrong type", `{"epoch":1}`, "text/html", 200},
		{"zero epoch", `{"epoch":0}`, "application/json", 200},
		{"redirect", "", "application/json", 307},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			c := clientWithPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			if _, err := c.State(context.Background()); err == nil {
				t.Fatal("accepted invalid response")
			}
			if calls.Load() != 1 {
				t.Fatal("followed redirect/retried", calls.Load())
			}
		})
	}
}

func TestClientDownloadVerification(t *testing.T) {
	base, err := delta.Build(context.Background(), bytes.NewReader(nil), 0, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	want := journal.Version{ID: identifier("selected"), Size: 4, Digest: hash.SumBytes([]byte("good"))}
	for _, test := range []struct {
		name, target, trailer string
		empty                 bool
	}{
		{"oversized target", "too large", "", true},
		{"wrong digest", "evil", "", false},
		{"failed trailer", "good", "source changed", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := encoded(t, nil, []byte(test.target))
			c := clientWithPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Trailer", "X-Ananas-Error")
				w.Write(wire)
				if test.trailer != "" {
					w.Header().Set("X-Ananas-Error", test.trailer)
				}
			}))
			var out bytes.Buffer
			if _, err := c.Download(context.Background(), Selection{"file", want.ID}, want, base, nil, &out); err == nil {
				t.Fatal("granted publication permission")
			}
			if test.empty && out.Len() != 0 {
				t.Fatal("wrote unapproved target size", out.Len())
			}
		})
	}
}

func TestClientInterruptAndBusy(t *testing.T) {
	entered := make(chan struct{})
	c := clientWithPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	done := make(chan error, 1)
	go func() { _, err := c.State(context.Background()); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	if _, err := c.State(context.Background()); err == nil {
		t.Fatal("queued concurrent work")
	}
	c.Interrupt()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt failed to cancel socket")
	}
}

func TestClientScopedRecovery(t *testing.T) {
	f := setup(t, true, nil)
	f.api.publisher = failAfterPublish{f.publisher}
	c := newTestClient(t, clientOptions(f))
	wire := encoded(t, nil, []byte("recover"))
	request, _ := f.request(t, "client-recovery", "file", "", []byte("recover"), wire)
	if _, err := c.Apply(context.Background(), request, []io.Reader{bytes.NewReader(wire)}); err == nil {
		t.Fatal("failure was acknowledged")
	}
	// Recover through the real publisher without mutating a live handler.
	if _, err := f.c.Recover(context.Background(), f.c.Epoch(), f.publisher); err != nil {
		t.Fatal(err)
	}
	record, err := c.Recover(context.Background(), RecoverRequest{f.c.Epoch(), request.Proposal.ID}, request.Proposal)
	if err != nil || !record.Committed {
		t.Fatal(record, err)
	}
}
