package transferapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/publish"
	"nas-sync/internal/stage"
)

func identifier(s string) string { return hash.SumBytes([]byte(s)).Hex() }

type fixture struct {
	root, state string
	c           *journal.Coordinator
	store       *stage.Store
	publisher   *publish.Publisher
	api         *Server
	server      *httptest.Server
	client      *http.Client
	clientID    string
}

func certificates(t *testing.T) (tls.Certificate, tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	issue := func(n int64, usage x509.ExtKeyUsage) tls.Certificate {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(n), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}
	return issue(2, x509.ExtKeyUsageServerAuth), issue(3, x509.ExtKeyUsageClientAuth), pool
}
func setup(t *testing.T, writes bool, patterns []string) *fixture {
	return setupWithClientID(t, writes, patterns, identifier("client"))
}

func setupWithClientID(t *testing.T, writes bool, patterns []string, clientID string) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{root: filepath.Join(dir, "root"), state: filepath.Join(dir, "state"), clientID: clientID}
	for _, path := range []string{f.root, f.state} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.c, err = journal.Open(f.state, identifier("namespace"))
	if err != nil {
		t.Fatal(err)
	}
	f.store, err = stage.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	f.publisher, err = publish.Open(f.root, f.state, f.c.Version, patterns, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, clientCert, pool := certificates(t)
	fingerprint := sha256.Sum256(clientCert.Certificate[0])
	f.api, err = New(f.c, f.store, f.publisher, Options{Clients: map[string]string{hex.EncodeToString(fingerprint[:]): f.clientID}, Exclusions: patterns, Writes: writes, MaxEventStreams: 3, MaxFileBytes: 1024 * 1024, MaxBatchBytes: 2 * 1024 * 1024, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewUnstartedServer(f.api)
	f.server.Listener, err = LimitListener(f.server.Listener, 4)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Config = HTTPServer(f.api)
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	f.server.StartTLS()
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{clientCert}}, MaxConnsPerHost: 2, DisableCompression: true}
	f.client = &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		f.server.Close()
		f.publisher.Close()
		f.store.Close()
		f.c.Close()
	})
	return f
}
func (f *fixture) call(t *testing.T, path string, body []byte) (int, []byte, http.Header) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Ananas-Namespace", f.c.Namespace())
	r, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	p, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, p, r.Trailer
}
func encoded(t *testing.T, base, target []byte) []byte {
	t.Helper()
	sig, err := delta.Build(context.Background(), bytes.NewReader(base), int64(len(base)), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := delta.Encode(context.Background(), bytes.NewReader(target), int64(len(target)), sig, &b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func (f *fixture) request(t *testing.T, op, path, expected string, target, wire []byte) (ApplyRequest, []byte) {
	t.Helper()
	v := journal.Version{ID: identifier(op + "version"), Size: int64(len(target)), Digest: hash.SumBytes(target)}
	request := ApplyRequest{Epoch: f.c.Epoch(), Proposal: journal.Proposal{ID: identifier(op), Client: f.clientID, Entries: []journal.Entry{{Path: path, Expected: expected, Next: v}}}, DeltaBytes: []int64{int64(len(wire))}}
	var b bytes.Buffer
	if err := WriteFrame(&b, request, journal.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	b.Write(wire)
	return request, b.Bytes()
}
func TestTLSDeltaPushAndPull(t *testing.T) {
	f := setup(t, true, nil)
	old := make([]byte, 256*1024)
	if _, err := rand.Read(old); err != nil {
		t.Fatal(err)
	}
	first, body := f.request(t, "create", "file", "", old, encoded(t, nil, old))
	if status, body, _ := f.call(t, "/v1/apply", body); status != 200 {
		t.Fatal(status, string(body))
	}
	var selection bytes.Buffer
	if err := WriteFrame(&selection, Selection{"file", first.Proposal.Entries[0].Next.ID}, 8192); err != nil {
		t.Fatal(err)
	}
	status, raw, _ := f.call(t, "/v1/signature", selection.Bytes())
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	sig, err := delta.ParseSignature(raw)
	if err != nil {
		t.Fatal(err)
	}
	next := append([]byte{99}, old...)
	var wire bytes.Buffer
	stats, err := delta.Encode(context.Background(), bytes.NewReader(next), int64(len(next)), sig, &wire)
	if err != nil || stats.LiteralBytes != 1 {
		t.Fatal(stats, err)
	}
	second, body := f.request(t, "replace", "file", first.Proposal.Entries[0].Next.ID, next, wire.Bytes())
	status, result, _ := f.call(t, "/v1/apply", body)
	if status != 200 {
		t.Fatal(status, string(result))
	}
	var committed ApplyResponse
	if err := json.Unmarshal(result, &committed); err != nil {
		t.Fatal(err)
	}
	if !committed.Record.Committed || committed.Transfers[0].LiteralBytes != 1 || committed.Transfers[0].ReusedBytes != int64(len(old)) {
		t.Fatal(committed)
	}
	got, err := os.ReadFile(filepath.Join(f.root, "file"))
	if err != nil || !bytes.Equal(got, next) {
		t.Fatal("published bytes differ", err)
	}
	// Retrying the same operation does not stage or publish a second time.
	status, result, _ = f.call(t, "/v1/apply", body)
	if status != 200 {
		t.Fatal(status, string(result))
	}
	baseSig, err := delta.Build(context.Background(), bytes.NewReader(old), int64(len(old)), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	selection.Reset()
	WriteFrame(&selection, Selection{"file", second.Proposal.Entries[0].Next.ID}, 8192)
	sigBytes, _ := baseSig.MarshalBinary()
	selection.Write(sigBytes)
	status, pull, trailer := f.call(t, "/v1/delta", selection.Bytes())
	if status != 200 || trailer.Get("X-Ananas-Error") != "" {
		t.Fatal(status, trailer, string(pull))
	}
	var out bytes.Buffer
	received, err := delta.Apply(context.Background(), bytes.NewReader(pull), bytes.NewReader(old), int64(len(old)), baseSig.Digest, &out)
	if err != nil || !bytes.Equal(out.Bytes(), next) || received.Digest != second.Proposal.Entries[0].Next.Digest || received.LiteralBytes != 1 {
		t.Fatal("download failed verification", received, err)
	}
}
func TestRejectIdentityScopeAndLimitsBeforeStaging(t *testing.T) {
	for _, kind := range []string{"read-only", "client", "excluded", "target-size", "wire-header", "epoch"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, kind != "read-only", []string{"excluded"})
			request, body := f.request(t, "put", "file", "", []byte("value"), encoded(t, nil, []byte("value")))
			var b bytes.Buffer
			switch kind {
			case "client":
				request.Proposal.Client = identifier("other-client")
			case "excluded":
				request.Proposal.Entries[0].Path = "excluded"
			case "target-size":
				request.Proposal.Entries[0].Next.Size = 2 * 1024 * 1024
			case "wire-header":
				request.Proposal.Entries[0].Next.Size = 6
			case "epoch":
				request.Epoch++
			}
			if kind != "read-only" {
				WriteFrame(&b, request, journal.MaxRecordBytes)
				b.Write(encoded(t, nil, []byte("value")))
				body = b.Bytes()
			}
			status, result, _ := f.call(t, "/v1/apply", body)
			if status == 200 {
				t.Fatal("unsafe request succeeded", string(result))
			}
			entries, err := os.ReadDir(f.root)
			if err != nil || len(entries) != 0 {
				t.Fatal("unsafe request published content", entries, err)
			}
			if _, err := os.Lstat(filepath.Join(f.state, request.Proposal.Entries[0].Next.ID+".ready")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsafe request reached staging", err)
			}
		})
	}
}
func TestMissingCertificateRejected(t *testing.T) {
	f := setup(t, true, nil)
	transport := f.client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get(f.server.URL + "/v1/state")
	if err == nil {
		response.Body.Close()
		t.Fatal("TLS accepted a client without certificate")
	}
}
func TestFramingBounds(t *testing.T) {
	for _, data := range [][]byte{{}, {0xff, 0xff, 0xff, 0xff}, {0, 0, 0, 10, '{', '}'}, {0, 0, 0, 2, '[', ']'}} {
		var request Selection
		if err := ReadFrame(bytes.NewReader(data), &request, 8192); err == nil {
			t.Fatal("invalid frame accepted")
		}
	}
	var b bytes.Buffer
	if err := WriteFrame(&b, map[string]string{"unknown": "field"}, 8192); err != nil {
		t.Fatal(err)
	}
	var selection Selection
	if err := ReadFrame(&b, &selection, 8192); err == nil {
		t.Fatal("unknown field accepted")
	}
}

type failAfterPublish struct{ p journal.Publisher }

func (f failAfterPublish) Publish(ctx context.Context, r journal.Record) error {
	if err := f.p.Publish(ctx, r); err != nil {
		return err
	}
	return errors.New("simulated response loss before journal commit")
}
func TestScopedRecovery(t *testing.T) {
	f := setup(t, true, nil)
	f.api.publisher = failAfterPublish{f.publisher}
	request, body := f.request(t, "put", "file", "", []byte("data"), encoded(t, nil, []byte("data")))
	status, _, _ := f.call(t, "/v1/apply", body)
	if status != 409 {
		t.Fatal(status)
	}
	f.api.publisher = f.publisher
	var b bytes.Buffer
	WriteFrame(&b, RecoverRequest{f.c.Epoch(), request.Proposal.ID}, 1024)
	status, result, _ := f.call(t, "/v1/recover", b.Bytes())
	if status != 200 {
		t.Fatal(status, string(result))
	}
	var record journal.Record
	if err := json.Unmarshal(result, &record); err != nil || !record.Committed {
		t.Fatal(record, err)
	}
	status, result, _ = f.call(t, "/v1/recover", b.Bytes())
	if status != 200 {
		t.Fatal("idempotent recovery failed", status, string(result))
	}
}
func TestBusyOwnerAndClosedGate(t *testing.T) {
	f := setup(t, true, nil)
	f.api.active <- struct{}{}
	status, _, _ := f.call(t, "/v1/signature", nil)
	if status != 503 {
		t.Fatal(status)
	}
	<-f.api.active
	f.api.opts.Gate = func(context.Context) error { return errors.New("LAN unavailable") }
	_, body := f.request(t, "put", "file", "", []byte("data"), encoded(t, nil, []byte("data")))
	status, result, _ := f.call(t, "/v1/apply", body)
	if status != 503 || !strings.Contains(string(result), "LAN unavailable") {
		t.Fatal(status, string(result))
	}
}

func TestDirectoryAndMultiFileBatch(t *testing.T) {
	f := setup(t, true, nil)
	directory := journal.Version{ID: identifier("directory"), Directory: true}
	request := ApplyRequest{Epoch: f.c.Epoch(), Proposal: journal.Proposal{ID: identifier("mkdir"), Client: f.clientID, Entries: []journal.Entry{{Path: "folder", Next: directory}}}, DeltaBytes: []int64{0}}
	var body bytes.Buffer
	WriteFrame(&body, request, journal.MaxRecordBytes)
	if status, result, _ := f.call(t, "/v1/apply", body.Bytes()); status != 200 {
		t.Fatal(status, string(result))
	}
	a, b := []byte("first"), []byte("second")
	aw, bw := encoded(t, nil, a), encoded(t, nil, b)
	request.Proposal.ID = identifier("files")
	request.Proposal.Entries = []journal.Entry{
		{Path: "folder/a", Next: journal.Version{ID: identifier("a"), Size: int64(len(a)), Digest: hash.SumBytes(a)}},
		{Path: "folder/b", Next: journal.Version{ID: identifier("b"), Size: int64(len(b)), Digest: hash.SumBytes(b)}},
	}
	request.DeltaBytes = []int64{int64(len(aw)), int64(len(bw))}
	body.Reset()
	WriteFrame(&body, request, journal.MaxRecordBytes)
	body.Write(aw)
	body.Write(bw)
	if status, result, _ := f.call(t, "/v1/apply", body.Bytes()); status != 200 {
		t.Fatal(status, string(result))
	}
	if records, err := f.c.Changes(0, 4); err != nil || len(records) != 2 || len(records[1].Entries) != 2 {
		t.Fatal(records, err)
	}
	for _, name := range []string{"a", "b"} {
		got, err := os.ReadFile(filepath.Join(f.root, "folder", name))
		if err != nil || len(got) == 0 {
			t.Fatal(got, err)
		}
	}
}

func TestAllowedCAIsNotEnoughWithoutCertificateMapping(t *testing.T) {
	f := setup(t, true, nil)
	f.api.opts.Clients = map[string]string{}
	status, _, _ := f.call(t, "/v1/signature", nil)
	if status != 401 {
		t.Fatal("unmapped verified certificate accepted", status)
	}
}

func TestConnectionLimitAndClose(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := LimitListener(base, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if connection != nil {
			connection.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatal("capacity was not enforced", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing listener did not unblock saturated accept")
	}
	accepted.Close() // repeated Close must release capacity only once
}

func TestPacingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pacer{ctx: ctx, rate: 1024, bytes: 64 * 1024, start: time.Now()}
	cancel()
	start := time.Now()
	if err := p.before(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation waited for the rate budget")
	}
}
