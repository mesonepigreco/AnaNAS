package transferapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/delta"
	"nas-sync/internal/exclude"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
	"nas-sync/internal/socketpolicy"
)

type ClientOptions struct {
	Namespace                   string
	Endpoint                    netip.AddrPort
	Source                      netip.Addr
	Interface                   string
	Roots                       *x509.CertPool
	Certificate                 tls.Certificate
	ServerFingerprint           string
	ClientID                    string
	Exclusions                  []string
	Writes                      bool
	MaxFileBytes, MaxBatchBytes int64
	Gate                        func(context.Context) error
	// RecordTraffic receives encrypted stream byte deltas; it must not perform I/O.
	RecordTraffic func(upload, download uint64)
}
type Traffic struct {
	Sent     uint64 `json:"sent"`
	Received uint64 `json:"received"`
}
type State struct {
	Epoch  uint64 `json:"epoch"`
	Writes bool   `json:"writes"`
	Scope  string `json:"scope,omitempty"`
}
type RemoteError struct {
	Status  int
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("NAS protocol status %d: %s", e.Status, e.Message)
}

// Retryable identifies temporary service failures without treating rejected
// credentials, conflicts, malformed input or missing versions as retry loops.
func (e *RemoteError) Retryable() bool {
	switch e.Status {
	case 408, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

type Client struct {
	opts             ClientOptions
	exclusions       *exclude.Matcher
	transport        *http.Transport
	http             *http.Client
	baseURL          string
	mu               sync.Mutex
	active, closed   bool
	cancel           context.CancelFunc
	watching         bool
	watchCancel      context.CancelFunc
	sent, received   atomic.Uint64
	changes          chan struct{}
	operationTimeout time.Duration
}

// NewClient permits only an IPv4 private literal (or explicit loopback tests),
// verified TLS 1.3, a pinned server leaf, and a bound source/interface. It never
// uses an HTTP proxy, resolves an endpoint name or follows redirects. The gate
// must supply the actual LAN/pause/egress policy; these socket choices alone do
// not prove that a route cannot change to a gateway on the same interface.
func NewClient(o ClientOptions) (*Client, error) {
	if !o.Endpoint.IsValid() || o.Endpoint.Port() == 0 || !o.Endpoint.Addr().Is4() || (!o.Endpoint.Addr().IsPrivate() && !o.Endpoint.Addr().IsLoopback()) {
		return nil, fmt.Errorf("private IPv4 endpoint literal required")
	}
	if !o.Source.Is4() || (!o.Source.IsPrivate() && !o.Source.IsLoopback()) || o.Source.IsLoopback() != o.Endpoint.Addr().IsLoopback() {
		return nil, fmt.Errorf("matching IPv4 source address required")
	}
	if o.Interface == "" || len(o.Interface) > 15 || strings.ContainsAny(o.Interface, "/\\\x00 \t\n") {
		return nil, fmt.Errorf("explicit interface required")
	}
	if o.Endpoint.Addr().IsLoopback() && o.Interface != "lo" {
		return nil, fmt.Errorf("loopback tests must bind lo")
	}
	if o.Roots == nil || len(o.Certificate.Certificate) == 0 || o.Certificate.PrivateKey == nil || !manifest.ValidID(o.ServerFingerprint) || !manifest.ValidID(o.ClientID) || !manifest.ValidID(o.Namespace) || o.Gate == nil {
		return nil, fmt.Errorf("trust, client certificate, server pin, client ID and policy gate required")
	}
	if o.MaxFileBytes < 1 || o.MaxFileBytes > 8<<30 || o.MaxBatchBytes < o.MaxFileBytes || o.MaxBatchBytes > 8<<30 {
		return nil, fmt.Errorf("bounded file and batch limits required")
	}
	o.Exclusions = append([]string(nil), o.Exclusions...)
	m, err := changefeed.Patterns(o.Exclusions)
	if err != nil {
		return nil, err
	}
	o.Roots = o.Roots.Clone()
	// The remote production helper is paced at 2 MiB/s. Allow up to eight
	// bounded content passes plus protocol overhead; metadata calls share the
	// same finite context so one active request remains interruptible.
	seconds := (o.MaxBatchBytes + (2 << 20) - 1) / (2 << 20)
	operationTimeout := 45*time.Second + time.Duration(seconds*8)*time.Second
	c := &Client{opts: o, exclusions: m, baseURL: "https://" + o.Endpoint.String(), changes: make(chan struct{}, 1), operationTimeout: operationTimeout}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: -1, LocalAddr: &net.TCPAddr{IP: net.IP(o.Source.AsSlice())}, Control: func(network, address string, raw syscall.RawConn) error {
		if network != "tcp4" || address != o.Endpoint.String() {
			return fmt.Errorf("unexpected dial destination")
		}
		var bindErr error
		err := raw.Control(func(fd uintptr) {
			bindErr = socketpolicy.BindIPv4(int(fd), o.Interface)
		})
		return errors.Join(err, bindErr)
	}}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: o.Roots, Certificates: []tls.Certificate{o.Certificate}, ServerName: o.Endpoint.Addr().String(), NextProtos: []string{"http/1.1"}, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return fmt.Errorf("verified NAS certificate required")
		}
		digest := sha256.Sum256(state.PeerCertificates[0].Raw)
		if hex.EncodeToString(digest[:]) != o.ServerFingerprint {
			return fmt.Errorf("NAS certificate pin changed")
		}
		return nil
	}}
	c.transport = &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig, ForceAttemptHTTP2: false, MaxConnsPerHost: 2, MaxIdleConnsPerHost: 1, MaxIdleConns: 1, IdleConnTimeout: 15 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: operationTimeout, DisableCompression: true, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != o.Endpoint.String() {
			return nil, fmt.Errorf("endpoint redirection refused")
		}
		if err := o.Gate(ctx); err != nil {
			return nil, err
		}
		conn, err := dialer.DialContext(ctx, "tcp4", address)
		if err != nil {
			return nil, err
		}
		return &countedConnection{Conn: conn, client: c}, nil
	}}
	c.http = &http.Client{Transport: c.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}

type countedConnection struct {
	net.Conn
	client *Client
}

func (c *countedConnection) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.client.received.Add(uint64(n))
		if record := c.client.opts.RecordTraffic; record != nil {
			record(0, uint64(n))
		}
		c.client.notify()
	}
	return n, err
}
func (c *countedConnection) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.client.sent.Add(uint64(n))
		if record := c.client.opts.RecordTraffic; record != nil {
			record(uint64(n), 0)
		}
		c.client.notify()
	}
	return n, err
}
func (c *Client) notify() {
	select {
	case c.changes <- struct{}{}:
	default:
	}
}

// Traffic counts encrypted TCP-stream bytes, including TLS handshakes/retries,
// but excluding TCP/IP headers and kernel retransmissions. Changes is a coalesced
// notification only; no worker or polling timer is started by reading it.
func (c *Client) Traffic() Traffic         { return Traffic{c.sent.Load(), c.received.Load()} }
func (c *Client) Changes() <-chan struct{} { return c.changes }
func (c *Client) Interrupt() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	if c.watchCancel != nil {
		c.watchCancel()
	}
	c.mu.Unlock()
	c.transport.CloseIdleConnections()
}
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	if c.watchCancel != nil {
		c.watchCancel()
	}
	c.mu.Unlock()
	c.transport.CloseIdleConnections()
	return nil
}
func (c *Client) begin(ctx context.Context) (context.Context, func(), error) {
	c.mu.Lock()
	if c.closed || c.active {
		c.mu.Unlock()
		return nil, nil, fmt.Errorf("client is closed or busy")
	}
	ctx, cancel := context.WithTimeout(ctx, c.operationTimeout)
	c.active = true
	c.cancel = cancel
	c.mu.Unlock()
	finish := func() { cancel(); c.mu.Lock(); c.active = false; c.cancel = nil; c.mu.Unlock() }
	if err := c.opts.Gate(ctx); err != nil {
		finish()
		return nil, nil, err
	}
	return ctx, finish, nil
}
func (c *Client) path(path string, isDir bool) error {
	if path == "." || len(path) > 4096 || !fs.ValidPath(path) || strings.ContainsAny(path, "\\\x00") || c.exclusions.Match(path, isDir) {
		return fmt.Errorf("invalid or excluded client path")
	}
	return nil
}
func (c *Client) request(ctx context.Context, method, path string, body io.Reader, length int64) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	r.ContentLength = length
	r.Header.Set("X-Ananas-Namespace", c.opts.Namespace)
	if body != nil {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	response, err := c.http.Do(r)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		var message struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &message)
		if message.Error == "" {
			message.Error = http.StatusText(response.StatusCode)
		}
		return nil, &RemoteError{response.StatusCode, message.Error}
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		response.Body.Close()
		return nil, fmt.Errorf("compressed protocol response refused")
	}
	return response, nil
}
func readJSON(response *http.Response, out any, max int64) error {
	if strings.Split(response.Header.Get("Content-Type"), ";")[0] != "application/json" {
		return fmt.Errorf("unexpected JSON content type")
	}
	p, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	if err != nil {
		return err
	}
	if int64(len(p)) > max {
		return fmt.Errorf("response exceeds metadata limit")
	}
	d := json.NewDecoder(bytes.NewReader(p))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing response JSON")
	}
	return nil
}
func (c *Client) State(ctx context.Context) (State, error) {
	var state State
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return state, err
	}
	defer finish()
	r, err := c.request(ctx, http.MethodGet, "/v1/state", nil, 0)
	if err != nil {
		return state, err
	}
	defer r.Body.Close()
	if err := readJSON(r, &state, 1024); err != nil {
		return state, err
	}
	if state.Epoch == 0 {
		return state, fmt.Errorf("invalid remote epoch")
	}
	return state, nil
}
func (c *Client) Head(ctx context.Context, path string) (HeadResponse, error) {
	var result HeadResponse
	if err := c.path(path, false); err != nil {
		return result, err
	}
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	r, err := c.request(ctx, http.MethodGet, "/v1/head?path="+url.QueryEscape(path), nil, 0)
	if err != nil {
		return result, err
	}
	defer r.Body.Close()
	if err := readJSON(r, &result, 8192); err != nil {
		return result, err
	}
	if result.Version != nil {
		if err := c.version(*result.Version); err != nil {
			return result, err
		}
		if err := c.path(path, result.Version.Directory); err != nil {
			return result, err
		}
	}
	return result, nil
}
func (c *Client) version(v journal.Version) error {
	if v.Size > c.opts.MaxFileBytes {
		return fmt.Errorf("remote version exceeds file limit")
	}
	return journal.ValidateVersion(v)
}

func (c *Client) selection(sel Selection, want journal.Version) error {
	if err := c.path(sel.Path, false); err != nil {
		return err
	}
	if err := c.version(want); err != nil {
		return err
	}
	if sel.Version != want.ID || want.Directory || want.Tombstone {
		return fmt.Errorf("selection does not match a file version")
	}
	return nil
}
func (c *Client) Signature(ctx context.Context, sel Selection, want journal.Version) (*delta.Signature, error) {
	if err := c.selection(sel, want); err != nil {
		return nil, err
	}
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	var frame bytes.Buffer
	if err := WriteFrame(&frame, sel, 8192); err != nil {
		return nil, err
	}
	r, err := c.request(ctx, http.MethodPost, "/v1/signature", &frame, int64(frame.Len()))
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		return nil, fmt.Errorf("unexpected signature content type")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, delta.MaxSignatureBytes+1))
	if err != nil {
		return nil, err
	}
	sig, err := delta.ParseSignature(data)
	if err != nil {
		return nil, err
	}
	if sig.BlockSize != 64*1024 || sig.Size != want.Size || sig.Digest != want.Digest {
		return nil, fmt.Errorf("signature differs from selected version")
	}
	return sig, nil
}

// Download writes to a caller-owned unpublished staging writer. Success requires
// complete delta verification, the independently expected target digest/size,
// the final HTTP trailer and a fresh gate check. Any error forbids publication.
func (c *Client) Download(ctx context.Context, sel Selection, want journal.Version, base *delta.Signature, source io.ReaderAt, staging io.Writer) (delta.Stats, error) {
	var result delta.Stats
	if err := c.selection(sel, want); err != nil {
		return result, err
	}
	if err := base.Validate(); err != nil {
		return result, err
	}
	if base.BlockSize != 64*1024 || base.Size > c.opts.MaxFileBytes || staging == nil {
		return result, fmt.Errorf("bounded base and staging output required")
	}
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	var frame bytes.Buffer
	if err := WriteFrame(&frame, sel, 8192); err != nil {
		return result, err
	}
	data, err := base.MarshalBinary()
	if err != nil {
		return result, err
	}
	frame.Write(data)
	r, err := c.request(ctx, http.MethodPost, "/v1/delta", &frame, int64(frame.Len()))
	if err != nil {
		return result, err
	}
	defer r.Body.Close()
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		return result, fmt.Errorf("unexpected delta content type")
	}
	max := want.Size + int64(delta.MaxOperations)*45 + delta.HeaderBytes + 33
	wire := bufio.NewReaderSize(io.LimitReader(r.Body, max+1), 4096)
	header, err := wire.Peek(delta.HeaderBytes)
	if err != nil {
		return result, err
	}
	h, err := delta.InspectHeader(header)
	if err != nil || h.BlockSize != base.BlockSize || h.BaseSize != base.Size || h.BaseDigest != base.Digest || h.TargetSize != want.Size {
		return result, fmt.Errorf("download header differs from approved version sizes/base")
	}
	result, err = delta.Apply(ctx, wire, source, base.Size, base.Digest, staging)
	if err != nil {
		return result, err
	}
	if result.Digest != want.Digest || result.LiteralBytes+result.ReusedBytes != want.Size || r.Trailer.Get("X-Ananas-Error") != "" {
		return result, fmt.Errorf("download differs from selected version or reports a failed trailer")
	}
	if err := c.opts.Gate(ctx); err != nil {
		return result, err
	}
	return result, nil
}

// DownloadVersion implements the replica downloader contract while retaining
// exact path/version selection and all normal download verification.
func (c *Client) DownloadVersion(ctx context.Context, path string, want journal.Version, base *delta.Signature, source io.ReaderAt, staging io.Writer) (delta.Stats, error) {
	return c.Download(ctx, Selection{Path: path, Version: want.ID}, want, base, source, staging)
}

// Apply consumes already prepared, bounded delta streams. It does not start a
// goroutine per file or buffer whole file bodies. Callers own immutable spools,
// source pacing, generation checks and retry budgets; the method never retries.
func (c *Client) Apply(ctx context.Context, request ApplyRequest, streams []io.Reader) (ApplyResponse, error) {
	var result ApplyResponse
	if !c.opts.Writes {
		return result, fmt.Errorf("client writes disabled")
	}
	if request.Epoch == 0 || request.Proposal.Client != c.opts.ClientID {
		return result, fmt.Errorf("invalid epoch or client identity")
	}
	if err := journal.ValidateProposal(request.Proposal); err != nil {
		return result, err
	}
	if len(streams) != len(request.Proposal.Entries) || len(request.DeltaBytes) != len(streams) {
		return result, fmt.Errorf("stream count differs from proposal")
	}
	var bytesTotal, wireTotal int64
	for i, e := range request.Proposal.Entries {
		if err := c.path(e.Path, e.Next.Directory || e.Next.Tombstone); err != nil {
			return result, err
		}
		if err := c.version(e.Next); err != nil {
			return result, err
		}
		if e.Next.Directory || e.Next.Tombstone {
			if request.DeltaBytes[i] != 0 || streams[i] != nil {
				return result, fmt.Errorf("metadata entry contains stream")
			}
			continue
		}
		if e.Next.Size > c.opts.MaxBatchBytes-bytesTotal {
			return result, fmt.Errorf("batch exceeds reconstructed byte limit")
		}
		bytesTotal += e.Next.Size
		length := request.DeltaBytes[i]
		if streams[i] == nil || length < delta.HeaderBytes+33 || length > e.Next.Size+int64(delta.MaxOperations)*45+delta.HeaderBytes+33 {
			return result, fmt.Errorf("invalid delta stream length")
		}
		wireTotal += length
	}
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	var frame bytes.Buffer
	if err := WriteFrame(&frame, request, journal.MaxRecordBytes); err != nil {
		return result, err
	}
	readers := make([]io.Reader, 0, len(streams)+1)
	readers = append(readers, &frame)
	for i, stream := range streams {
		if stream != nil {
			readers = append(readers, io.LimitReader(stream, request.DeltaBytes[i]))
		}
	}
	length := int64(frame.Len()) + wireTotal
	r, err := c.request(ctx, http.MethodPost, "/v1/apply", io.MultiReader(readers...), length)
	if err != nil {
		return result, err
	}
	defer r.Body.Close()
	if err := readJSON(r, &result, 2*journal.MaxRecordBytes); err != nil {
		return result, err
	}
	if !result.Record.Committed || result.Record.Sequence == 0 || result.Record.Epoch == 0 || !reflect.DeepEqual(result.Record.Proposal, request.Proposal) {
		return result, fmt.Errorf("commit acknowledgement differs from proposed operation")
	}
	return result, nil
}

func (c *Client) Recover(ctx context.Context, request RecoverRequest, want journal.Proposal) (*journal.Record, error) {
	if !c.opts.Writes || request.Epoch == 0 || request.Operation != want.ID || want.Client != c.opts.ClientID {
		return nil, fmt.Errorf("recovery identity or write gate differs")
	}
	if err := journal.ValidateProposal(want); err != nil {
		return nil, err
	}
	for _, e := range want.Entries {
		if err := c.path(e.Path, e.Next.Directory || e.Next.Tombstone); err != nil {
			return nil, err
		}
	}
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	var frame bytes.Buffer
	if err := WriteFrame(&frame, request, 1024); err != nil {
		return nil, err
	}
	r, err := c.request(ctx, http.MethodPost, "/v1/recover", &frame, int64(frame.Len()))
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var record journal.Record
	if err := readJSON(r, &record, journal.MaxRecordBytes); err != nil {
		return nil, err
	}
	if !record.Committed || record.Sequence == 0 || record.Epoch == 0 || !reflect.DeepEqual(record.Proposal, want) {
		return nil, fmt.Errorf("recovery acknowledgement differs from expected operation")
	}
	return &record, nil
}
