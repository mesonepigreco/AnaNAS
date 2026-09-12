// Package transferapi exposes the native transfer primitives over authenticated
// HTTP. It does not create a listener, schedule work or authorize LAN egress.
// Production writes must remain disabled until the deployment gates pass.
package transferapi

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/delta"
	"nas-sync/internal/exclude"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
	"nas-sync/internal/stage"
)

type Options struct {
	// Map SHA-256 of an allowed verified leaf certificate to its client ID.
	Clients        map[string]string
	Exclusions     []string
	Writes         bool
	DisposableTest bool
	// Zero disables notification streams. Deployment must reserve at least one
	// accepted connection for transfers beyond this 0..4 stream allowance.
	MaxEventStreams             int
	MaxFileBytes, MaxBatchBytes int64
	ReadBytesPerSecond          int64
	// Gate must recheck deployment/LAN policy before any operation. Tests use
	// an explicit local gate. A nil gate is refused, not interpreted as allow.
	Gate func(context.Context) error
}
type Server struct {
	c          *journal.Coordinator
	store      *stage.Store
	publisher  journal.Publisher
	opts       Options
	exclusions *exclude.Matcher
	active     chan struct{}
	lifecycle  sync.Mutex
	stopping   bool
	requests   sync.WaitGroup
	streamsMu  sync.Mutex
	streams    map[string]bool
}
type ApplyRequest struct {
	Epoch    uint64           `json:"epoch"`
	Proposal journal.Proposal `json:"proposal"`
	// One wire length per entry; zero for directories/tombstones. Delta streams
	// follow this framed JSON record in proposal order, exactly these lengths.
	DeltaBytes []int64 `json:"deltaBytes"`
}
type ApplyResponse struct {
	Record    journal.Record `json:"record"`
	Transfers []delta.Stats  `json:"transfers"`
}
type Selection struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}
type HeadResponse struct {
	Version *journal.Version `json:"version"`
	Pending bool             `json:"pending"`
}
type RecoverRequest struct {
	Epoch     uint64 `json:"epoch"`
	Operation string `json:"operation"`
}

func New(c *journal.Coordinator, s *stage.Store, p journal.Publisher, o Options) (*Server, error) {
	if c == nil || s == nil || p == nil || o.Gate == nil {
		return nil, fmt.Errorf("coordinator, store, publisher and policy gate required")
	}
	if err := c.CheckStore(s); err != nil {
		return nil, err
	}
	if len(o.Clients) < 1 || len(o.Clients) > 64 {
		return nil, fmt.Errorf("one to 64 certificate identities required")
	}
	clients := make(map[string]string, len(o.Clients))
	for fp, id := range o.Clients {
		if !manifest.ValidID(fp) || !manifest.ValidID(id) {
			return nil, fmt.Errorf("invalid certificate fingerprint or client ID")
		}
		clients[fp] = id
	}
	o.Clients = clients
	if o.MaxFileBytes < 1 || o.MaxFileBytes > 8<<30 || o.MaxBatchBytes < o.MaxFileBytes || o.MaxBatchBytes > 8<<30 {
		return nil, fmt.Errorf("file/batch byte bounds required (maximum 8 GiB)")
	}
	if o.ReadBytesPerSecond < 1024 || o.ReadBytesPerSecond > 1<<30 {
		return nil, fmt.Errorf("bounded transfer read rate required")
	}
	if o.MaxEventStreams < 0 || o.MaxEventStreams > 4 {
		return nil, fmt.Errorf("notification stream limit must be in [0,4]")
	}
	o.Exclusions = append([]string(nil), o.Exclusions...)
	m, err := changefeed.Patterns(o.Exclusions)
	if err != nil {
		return nil, err
	}
	return &Server{c: c, store: s, publisher: p, opts: o, exclusions: m, active: make(chan struct{}, 1), streams: make(map[string]bool)}, nil
}
func (s *Server) allowedPath(path string, isDir bool) error {
	if path == "." || len(path) > 4096 || !fs.ValidPath(path) || strings.ContainsAny(path, "\\\x00") {
		return fmt.Errorf("invalid path")
	}
	if s.exclusions.Match(path, isDir) {
		return fmt.Errorf("excluded path")
	}
	return nil
}
func (s *Server) client(r *http.Request) (string, error) {
	if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return "", fmt.Errorf("verified client certificate required")
	}
	fp := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
	id := s.opts.Clients[hex.EncodeToString(fp[:])]
	if id == "" {
		return "", fmt.Errorf("client certificate is not allowed")
	}
	return id, nil
}
func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, err error) {
	reply(w, status, struct {
		Error string `json:"error"`
	}{err.Error()})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.lifecycle.Lock()
	if s.stopping {
		s.lifecycle.Unlock()
		http.Error(w, "service stopping", http.StatusServiceUnavailable)
		return
	}
	s.requests.Add(1)
	s.lifecycle.Unlock()
	defer s.requests.Done()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.ProtoMajor != 1 {
		fail(w, http.StatusHTTPVersionNotSupported, fmt.Errorf("HTTP/1 transport required"))
		return
	}
	id, err := s.client(r)
	if err != nil {
		fail(w, http.StatusUnauthorized, err)
		return
	}
	if r.Header.Get("X-Ananas-Namespace") != s.c.Namespace() {
		fail(w, http.StatusConflict, fmt.Errorf("configured namespace differs"))
		return
	}
	// A bounded notification connection must not occupy the content-operation
	// slot. It shares authentication, namespace checks and the shutdown barrier.
	if r.Method == http.MethodGet && r.URL.Path == "/v1/events" {
		s.events(w, r, id)
		return
	}
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	default:
		fail(w, http.StatusServiceUnavailable, fmt.Errorf("transfer owner busy; retry within the operation's budget"))
		return
	}
	seconds := (s.opts.MaxBatchBytes + s.opts.ReadBytesPerSecond - 1) / s.opts.ReadBytesPerSecond
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second+time.Duration(seconds*8)*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	pace := &pacer{ctx: ctx, rate: s.opts.ReadBytesPerSecond, start: time.Now()}
	r.Body = &pacedBody{ReadCloser: r.Body, pace: pace}
	r = r.WithContext(context.WithValue(r.Context(), paceKey{}, pace))
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		fail(w, 500, err)
		return
	}
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		fail(w, 500, err)
		return
	}
	defer controller.SetReadDeadline(time.Time{})
	defer controller.SetWriteDeadline(time.Time{})
	if err := s.opts.Gate(ctx); err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/operation":
		s.operation(w, r, id)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/state":
		scope := ""
		if s.opts.DisposableTest {
			scope = "disposable-test"
		}
		reply(w, 200, struct {
			Epoch  uint64 `json:"epoch"`
			Writes bool   `json:"writes"`
			Scope  string `json:"scope,omitempty"`
		}{s.c.Epoch(), s.opts.Writes, scope})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/head":
		path := r.URL.Query().Get("path")
		if err := s.allowedPath(path, false); err != nil {
			fail(w, 400, err)
			return
		}
		v, pending, err := s.c.Head(path)
		if err != nil {
			fail(w, 500, err)
			return
		}
		if v != nil && v.Directory {
			if err := s.allowedPath(path, true); err != nil {
				fail(w, 403, err)
				return
			}
		}
		reply(w, 200, HeadResponse{v, pending})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/changes":
		s.changes(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/ack":
		s.acknowledge(w, r, id)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/signature":
		s.signature(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/delta":
		s.download(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/apply":
		if !s.opts.Writes {
			fail(w, http.StatusServiceUnavailable, fmt.Errorf("writes disabled until validation gates pass"))
			return
		}
		s.apply(w, r, id)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/recover":
		if !s.opts.Writes {
			fail(w, 503, fmt.Errorf("writes disabled until validation gates pass"))
			return
		}
		s.recover(w, r, id)
	default:
		fail(w, http.StatusNotFound, fmt.Errorf("unsupported operation"))
	}
}

// StopAndWait prevents new application operations and joins every admitted
// request before native state can close. The deployment first cancels request
// contexts and closes/shuts down HTTP connections; this barrier does no I/O and
// cannot forcibly interrupt an uninterruptible filesystem syscall.
func (s *Server) StopAndWait() {
	s.lifecycle.Lock()
	s.stopping = true
	s.lifecycle.Unlock()
	s.requests.Wait()
}

// JSON frames have a four-byte length prefix and strict bounded decoding. No
// streaming JSON decoder can prefetch delta bytes across the frame boundary.
func ReadFrame(r io.Reader, value any, max int) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || uint64(n) > uint64(max) {
		return fmt.Errorf("invalid metadata frame size")
	}
	data := make([]byte, int(n))
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing metadata JSON")
	}
	return nil
}
func WriteFrame(w io.Writer, value any, max int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > max {
		return fmt.Errorf("metadata frame exceeds limit")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if n, err := w.Write(header[:]); err != nil {
		return err
	} else if n != 4 {
		return io.ErrShortWrite
	}
	if n, err := w.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}
func eof(r io.Reader) error {
	var extra [1]byte
	n, err := io.ReadFull(r, extra[:])
	if n == 0 && err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected trailing request bytes")
}

func (s *Server) selected(sel Selection) (*journal.Version, error) {
	if err := s.allowedPath(sel.Path, false); err != nil {
		return nil, err
	}
	if !manifest.ValidID(sel.Version) {
		return nil, fmt.Errorf("invalid selected version")
	}
	v, pending, err := s.c.VersionAt(sel.Path, sel.Version)
	if err != nil {
		return nil, err
	}
	if pending {
		return nil, journal.ErrPending
	}
	if v == nil || v.ID != sel.Version {
		return nil, journal.ErrConflict
	}
	if v.Directory || v.Tombstone {
		return nil, fmt.Errorf("selected version is not file content")
	}
	if v.Size > s.opts.MaxFileBytes {
		return nil, fmt.Errorf("selected file exceeds operation limit")
	}
	return v, nil
}
func (s *Server) signature(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8196)
	var sel Selection
	if err := ReadFrame(r.Body, &sel, 8192); err != nil {
		fail(w, 400, err)
		return
	}
	if err := eof(r.Body); err != nil {
		fail(w, 400, err)
		return
	}
	v, err := s.selected(sel)
	if err != nil {
		fail(w, 409, err)
		return
	}
	f, err := s.store.OpenReady(v.ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer f.Close()
	sig, err := delta.Build(r.Context(), &pacedReader{r: f, pace: requestPacer(r)}, v.Size, 64*1024)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if sig.Digest != v.Digest {
		fail(w, 409, fmt.Errorf("retained version changed"))
		return
	}
	data, err := sig.MarshalBinary()
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(8196+delta.MaxSignatureBytes))
	var sel Selection
	if err := ReadFrame(r.Body, &sel, 8192); err != nil {
		fail(w, 400, err)
		return
	}
	v, err := s.selected(sel)
	if err != nil {
		fail(w, 409, err)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, 400, err)
		return
	}
	sig, err := delta.ParseSignature(data)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if sig.Size > s.opts.MaxFileBytes {
		fail(w, 400, fmt.Errorf("base exceeds operation limit"))
		return
	}
	if sig.BlockSize != 64*1024 {
		fail(w, 400, fmt.Errorf("transport requires 64 KiB blocks"))
		return
	}
	f, err := s.store.OpenReady(v.ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", "X-Ananas-Error")
	stats, err := delta.Encode(r.Context(), &pacedReader{r: f, pace: requestPacer(r)}, v.Size, sig, w)
	if err != nil || stats.Digest != v.Digest {
		w.Header().Set("X-Ananas-Error", "retained content or delta verification failed")
	}
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request, client string) {
	// The reconstructed target budget is separate from wire length; tiny copy
	// frames must not bypass the amount of storage permitted for this batch.
	maxWire := s.opts.MaxBatchBytes + int64(journal.MaxEntries)*(int64(delta.MaxOperations)*45+delta.HeaderBytes+33) + journal.MaxRecordBytes + 4
	r.Body = http.MaxBytesReader(w, r.Body, maxWire)
	var request ApplyRequest
	if err := ReadFrame(r.Body, &request, journal.MaxRecordBytes); err != nil {
		fail(w, 400, err)
		return
	}
	p := request.Proposal
	if request.Epoch != s.c.Epoch() {
		fail(w, 409, journal.ErrFence)
		return
	}
	if p.Client != client {
		fail(w, 403, fmt.Errorf("proposal client does not match authenticated identity"))
		return
	}
	if err := journal.ValidateProposal(p); err != nil {
		fail(w, 400, err)
		return
	}
	if len(request.DeltaBytes) != len(p.Entries) {
		fail(w, 400, fmt.Errorf("delta count differs from entry count"))
		return
	}
	for _, e := range p.Entries {
		if err := s.allowedPath(e.Path, e.Next.Directory || e.Next.Tombstone); err != nil {
			fail(w, 403, err)
			return
		}
	}
	prior, err := s.c.Operation(p.ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if prior != nil {
		if !reflect.DeepEqual(prior.Proposal, p) {
			fail(w, 409, journal.ErrIdentity)
			return
		}
		if !prior.Committed {
			fail(w, 409, journal.ErrPending)
			return
		}
		reply(w, 200, ApplyResponse{Record: *prior})
		return
	}
	var total int64
	bases := make([]*journal.Version, len(p.Entries))
	for i, e := range p.Entries {
		if err := s.allowedPath(e.Path, e.Next.Directory); err != nil {
			fail(w, 403, err)
			return
		}
		v, pending, err := s.c.Head(e.Path)
		if err != nil {
			fail(w, 500, err)
			return
		}
		if pending {
			fail(w, 409, journal.ErrPending)
			return
		}
		if v != nil && v.Directory {
			if err := s.allowedPath(e.Path, true); err != nil {
				fail(w, 403, err)
				return
			}
		}
		current := ""
		if v != nil {
			current = v.ID
		}
		if current != e.Expected {
			fail(w, 409, journal.ErrConflict)
			return
		}
		bases[i] = v
		if e.Next.Directory || e.Next.Tombstone {
			if request.DeltaBytes[i] != 0 {
				fail(w, 400, fmt.Errorf("metadata operation has delta payload"))
				return
			}
			continue
		}
		if e.Next.Size > s.opts.MaxFileBytes || e.Next.Size > s.opts.MaxBatchBytes-total {
			fail(w, 413, fmt.Errorf("reconstructed content exceeds operation limit"))
			return
		}
		total += e.Next.Size
		if request.DeltaBytes[i] < delta.HeaderBytes+33 || request.DeltaBytes[i] > e.Next.Size+int64(delta.MaxOperations)*45+delta.HeaderBytes+33 {
			fail(w, 400, fmt.Errorf("delta wire length out of bounds"))
			return
		}
	}
	stats := make([]delta.Stats, len(p.Entries))
	record, err := s.c.StageAndCommit(r.Context(), request.Epoch, p, func(ctx context.Context, resume bool) error {
		var err error
		stats, err = s.receiveUpload(r, request, bases, resume)
		if err != nil {
			return err
		}
		if err := eof(r.Body); err != nil {
			return err
		}
		return s.opts.Gate(ctx)
	}, s.publisher)
	if err != nil {
		if errors.Is(err, journal.ErrBudget) {
			fail(w, http.StatusInsufficientStorage, err)
			return
		}
		fail(w, 409, err)
		return
	}
	reply(w, 200, ApplyResponse{record, stats})
}

// Called only under the coordinator's exclusive durable intake ownership.
func (s *Server) receiveUpload(r *http.Request, request ApplyRequest, bases []*journal.Version, resume bool) ([]delta.Stats, error) {
	p := request.Proposal
	stats := make([]delta.Stats, len(p.Entries))
	for i, e := range p.Entries {
		if e.Next.Directory || e.Next.Tombstone {
			continue
		}
		if err := s.opts.Gate(r.Context()); err != nil {
			return nil, err
		}
		limited := &io.LimitedReader{R: r.Body, N: request.DeltaBytes[i]}
		wire := bufio.NewReaderSize(limited, 4096)
		header, err := wire.Peek(delta.HeaderBytes)
		if err != nil {
			return nil, err
		}
		h, err := delta.InspectHeader(header)
		baseSize := int64(0)
		baseDigest := hash.SumBytes(nil)
		var base *os.File
		v := bases[i]
		if v != nil && !v.Tombstone {
			if v.Directory {
				return nil, fmt.Errorf("directory base cannot reconstruct file")
			}
			baseSize, baseDigest = v.Size, v.Digest
		}
		if err != nil || h.BlockSize != 64*1024 || h.TargetSize != e.Next.Size || h.BaseSize != baseSize || h.BaseDigest != baseDigest {
			return nil, fmt.Errorf("delta header differs from approved version sizes/base")
		}
		if resume {
			// No earlier callback is live under this coordinator lock. Only
			// this exact intake can clean its interrupted partial names.
			if err := s.store.DiscardPartial(e.Next.ID); err != nil {
				return nil, err
			}
			reused, err := s.reuseCandidate(r, e.Next)
			if err != nil {
				return nil, err
			}
			if reused {
				// This protocol still resends the bounded wire spool. Consume
				// it to retain framing; the owned candidate supplies the bytes.
				if _, err := io.Copy(io.Discard, wire); err != nil {
					return nil, err
				}
				if limited.N != 0 {
					return nil, io.ErrUnexpectedEOF
				}
				stats[i].Digest = e.Next.Digest
				continue
			}
		}
		if v != nil && !v.Tombstone {
			base, err = s.store.OpenReady(v.ID)
			if err != nil {
				return nil, err
			}
		}
		var source io.ReaderAt
		if base != nil {
			source = &pacedReaderAt{r: base, pace: requestPacer(r)}
		}
		stats[i], err = s.store.ReceiveDeltaVerified(r.Context(), e.Next.ID, wire, source, baseSize, baseDigest, e.Next.Size, e.Next.Digest)
		if base != nil {
			err = errors.Join(err, base.Close())
		}
		if err != nil {
			return nil, err
		}
		if limited.N != 0 || stats[i].Digest != e.Next.Digest {
			return nil, fmt.Errorf("received content differs from proposed version")
		}
	}
	return stats, nil
}

func (s *Server) reuseCandidate(r *http.Request, v journal.Version) (bool, error) {
	f, err := s.store.OpenReady(v.ID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() != v.Size {
		return false, fmt.Errorf("owned candidate size differs")
	}
	digest, err := hash.Sum(io.LimitReader(&pacedReader{f, requestPacer(r)}, v.Size+1))
	if err != nil {
		return false, err
	}
	if digest != v.Digest {
		return false, fmt.Errorf("owned candidate digest differs")
	}
	return true, nil
}

type scopedRecovery struct {
	publisher         journal.Publisher
	operation, client string
	paths             func(string, bool) error
}

func (p scopedRecovery) Publish(ctx context.Context, r journal.Record) error {
	if r.ID != p.operation || r.Client != p.client {
		return journal.ErrIdentity
	}
	for _, e := range r.Entries {
		if err := p.paths(e.Path, e.Next.Directory || e.Next.Tombstone); err != nil {
			return err
		}
	}
	return p.publisher.Publish(ctx, r)
}
func (s *Server) recover(w http.ResponseWriter, r *http.Request, client string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1028)
	var request RecoverRequest
	if err := ReadFrame(r.Body, &request, 1024); err != nil {
		fail(w, 400, err)
		return
	}
	if err := eof(r.Body); err != nil {
		fail(w, 400, err)
		return
	}
	prior, err := s.c.Operation(request.Operation)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if prior == nil || prior.Client != client {
		fail(w, 403, fmt.Errorf("operation does not belong to authenticated client"))
		return
	}
	for _, e := range prior.Entries {
		if err := s.allowedPath(e.Path, e.Next.Directory || e.Next.Tombstone); err != nil {
			fail(w, 403, err)
			return
		}
	}
	if request.Epoch != s.c.Epoch() {
		fail(w, 409, journal.ErrFence)
		return
	}
	if prior.Committed {
		reply(w, 200, prior)
		return
	}
	if err := s.opts.Gate(r.Context()); err != nil {
		fail(w, 503, err)
		return
	}
	done, err := s.c.Recover(r.Context(), request.Epoch, scopedRecovery{s.publisher, request.Operation, client, s.allowedPath})
	if err != nil {
		fail(w, 409, err)
		return
	}
	if done == nil {
		fail(w, 409, journal.ErrPending)
		return
	}
	reply(w, 200, done)
}
