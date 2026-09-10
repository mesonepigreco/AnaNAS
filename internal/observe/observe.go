// Package observe provides local-only recursive observation. It never hashes
// file contents, accesses the NAS, or interprets an incomplete scan as deletion.
package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"nas-sync/internal/coalesce"
	"nas-sync/internal/config"
	"nas-sync/internal/exclude"
	"nas-sync/internal/index"
)

type Status struct {
	Mode          string `json:"mode"`
	Root          string `json:"root"`
	Ready         bool   `json:"ready"`
	Scanning      bool   `json:"scanning"`
	Events        uint64 `json:"events"`
	Batches       uint64 `json:"batches"`
	Scans         uint64 `json:"scans"`
	MetadataReads uint64 `json:"metadataReads"`
	Watches       int    `json:"watches"`
	LastError     string `json:"lastError,omitempty"`
}

type Observer struct {
	cfg        *config.Config
	root       *os.Root
	watcher    *fsnotify.Watcher
	matcher    *exclude.Matcher
	db         *index.DB
	watched    map[string]index.Fingerprint // scanner goroutine owns this map
	mu         sync.RWMutex
	status     Status
	changes    chan struct{}
	reconciled chan struct{}
	events     atomic.Uint64
	nextIO     time.Time
}

func New(cfg *config.Config, db *index.DB) (*Observer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newObserver(cfg, db)
}

// NewNative observes a NAS-native root already pinned by the helper publisher.
// It has no remote mount configuration and performs metadata work only. Defaults
// are 50 metadata operations/s, 8192 watches and 1.5–5 second coalescing.
func NewNative(root string, patterns []string, db *index.DB) (*Observer, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || db == nil {
		return nil, fmt.Errorf("canonical native root and index required")
	}
	cfg := config.Default()
	cfg.Local.Root = root
	cfg.Selective.ExcludeLocal = append([]string(nil), patterns...)
	cfg.Limits.MaxWatches = 8192
	o, err := newObserver(cfg, db)
	if err == nil {
		o.status.Mode = "native NAS metadata observation"
	}
	return o, err
}

func newObserver(cfg *config.Config, db *index.DB) (*Observer, error) {
	root, err := os.OpenRoot(cfg.Local.Root)
	if err != nil {
		return nil, err
	}
	// Apply both sides' exclusions: no automatic job may touch an excluded endpoint.
	patterns := append(append([]string{}, cfg.Selective.ExcludeLocal...), cfg.Selective.ExcludeRemote...)
	m, err := exclude.Compile(patterns)
	if err != nil {
		root.Close()
		return nil, err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		root.Close()
		return nil, err
	}
	return &Observer{cfg: cfg, root: root, watcher: w, matcher: m, db: db, watched: map[string]index.Fingerprint{}, changes: make(chan struct{}, 1), reconciled: make(chan struct{}, 1), status: Status{Mode: "local observation; NAS synchronization not enabled", Root: cfg.Local.Root}}, nil
}
func (o *Observer) Status() Status {
	o.mu.RLock()
	s := o.status
	o.mu.RUnlock()
	s.Events = o.events.Load()
	return s
}

// Changes is a bounded hint to refresh status, never a queue of file events.
func (o *Observer) Changes() <-chan struct{} { return o.changes }

// Reconciled hints that a successful metadata batch is durable. It is separate
// from UI status changes so a transfer worker never wakes once per scanned file.
// The durable dirty index, not this coalesced hint, is the source of pending work.
func (o *Observer) Reconciled() <-chan struct{} { return o.reconciled }
func (o *Observer) notifyReconciled() {
	select {
	case o.reconciled <- struct{}{}:
	default:
	}
}
func (o *Observer) set(f func(*Status)) {
	o.mu.Lock()
	f(&o.status)
	o.mu.Unlock()
	select {
	case o.changes <- struct{}{}:
	default:
	}
}

// Run always reconciles at startup, covering events missed while stopped. A
// persistent recovery marker is set for the entire running session and only
// retained on shutdown because cancellation may discard pending events.
func (o *Observer) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer o.root.Close()
	if err := o.db.SetRecovery(true); err != nil {
		o.watcher.Close()
		return err
	}
	in := make(chan string, 256)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() { defer close(done); defer close(in); o.ingest(ctx, in, errs) }()
	defer func() { cancel(); o.watcher.Close(); <-done }()
	cc := o.cfg.Coalesce
	batches := coalesce.New(coalesce.Config{Idle: cc.Idle.Std(), MaxWait: cc.MaxWait.Std(), MaxPending: cc.MaxPending, MaxBytes: cc.MaxBytes}).Run(ctx, in)
	if err := o.scan(ctx, ".", false); err != nil {
		return o.fail(err)
	}
	o.set(func(s *Status) { s.Ready = true })
	o.notifyReconciled()
	log.Print("local index ready; watching for changes")
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			if err != nil {
				return o.fail(err)
			}
		case b, ok := <-batches:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return o.fail(fmt.Errorf("watcher stopped"))
			}
			o.set(func(s *Status) { s.Batches++ })
			if b.Rescan {
				if err := o.scan(ctx, ".", false); err != nil {
					return o.fail(err)
				}
				o.notifyReconciled()
				continue
			}
			if err := o.scanBatch(ctx, b.Paths, true); err != nil {
				return o.fail(err)
			}
			o.notifyReconciled()
		}
	}
}
func (o *Observer) fail(err error) error {
	o.set(func(s *Status) { s.Ready = false; s.LastError = err.Error() })
	return err
}
func (o *Observer) ingest(ctx context.Context, out chan<- string, errs chan<- error) {
	// Never wait for the scanner. Coalescer drains this queue; if it also stalls,
	// one pending gap replaces any number of paths and is sent when space returns.
	gap := false
	for {
		var send chan<- string
		if gap {
			send = out
		}
		select {
		case <-ctx.Done():
			return
		case send <- coalesce.RescanPath:
			gap = false
		case err, ok := <-o.watcher.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				gap = true
				continue
			}
			select {
			case errs <- err:
			case <-ctx.Done():
			}
			return
		case ev, ok := <-o.watcher.Events:
			if !ok {
				return
			}
			rel, err := filepath.Rel(o.cfg.Local.Root, ev.Name)
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				continue
			}
			rel = filepath.ToSlash(rel)
			if o.matcher.Match(rel, false) {
				continue
			}
			o.events.Add(1)
			select {
			case out <- rel:
			default:
				gap = true
			}
		}
	}
}
func (o *Observer) pace(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay := time.Until(o.nextIO); delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	o.nextIO = time.Now().Add(time.Second / time.Duration(o.cfg.Limits.ScanOpsPerSecond))
	return nil
}
func (o *Observer) scan(ctx context.Context, rel string, force bool) error {
	return o.scanBatch(ctx, []string{rel}, force)
}
func (o *Observer) scanBatch(ctx context.Context, paths []string, force bool) error {
	// Coalescer paths are sorted. Collapse covered descendants without allocating
	// an additional tree or scanning a new directory once for every child event.
	roots := make([]string, 0, len(paths))
	covered := make(map[string]bool, len(paths))
	for _, p := range paths {
		ancestor := p
		skip := covered["."]
		for !skip {
			if covered[ancestor] {
				skip = true
				break
			}
			i := strings.LastIndexByte(ancestor, '/')
			if i < 0 {
				break
			}
			ancestor = ancestor[:i]
		}
		if skip {
			continue
		}
		roots = append(roots, p)
		covered[p] = true
	}
	o.set(func(s *Status) { s.Scanning = true; s.Scans++ })
	defer o.set(func(s *Status) { s.Scanning = false })
	// A renamed/replaced root must be unavailable, not mistaken for an empty tree.
	checkRoot := func() error {
		actual, err := os.Stat(o.cfg.Local.Root)
		if err != nil {
			return err
		}
		opened, err := o.root.Stat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(actual, opened) {
			return fmt.Errorf("local root was replaced; restart observation")
		}
		return nil
	}
	if err := checkRoot(); err != nil {
		return err
	}
	id, err := o.db.NextScan()
	if err != nil {
		return err
	}
	pending := make([]index.Record, 0, 128)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		err := o.db.Put(pending, force)
		pending = pending[:0]
		return err
	}
	var walk func(string, int) error
	walk = func(p string, depth int) error {
		if depth > 128 {
			return fmt.Errorf("directory depth exceeds 128 at %s", p)
		}
		if err := o.pace(ctx); err != nil {
			return err
		}
		info, err := o.root.Lstat(p)
		o.set(func(s *Status) { s.MetadataReads++ })
		if errors.Is(err, os.ErrNotExist) && p != "." {
			return nil
		}
		if err != nil {
			return err
		}
		if o.matcher.Match(p, info.IsDir()) {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			// Retain an excluded record so unsupported content cannot become a deletion.
			pending = append(pending, index.Record{Path: p, Excluded: true, Scan: id})
			if len(pending) == cap(pending) {
				return flush()
			}
			return nil
		}
		st := info.Sys().(*syscall.Stat_t)
		if p != "." {
			pending = append(pending, index.Record{Path: p, Scan: id, Fingerprint: index.Fingerprint{Device: uint64(st.Dev), Inode: st.Ino, Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), CtimeNS: st.Ctim.Nano(), Mode: uint32(info.Mode())}})
			if len(pending) == cap(pending) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if !info.IsDir() {
			return nil
		}
		// Remove stale watches below a renamed/deleted path during reconciliation.
		oldWatch, watched := o.watched[p]
		if watched && (oldWatch.Device != uint64(st.Dev) || oldWatch.Inode != st.Ino) {
			_ = o.watcher.Remove(filepath.Join(o.cfg.Local.Root, p))
			delete(o.watched, p)
			watched = false
		}
		if !watched {
			if len(o.watched) >= o.cfg.Limits.MaxWatches {
				return fmt.Errorf("watch limit reached (%d)", o.cfg.Limits.MaxWatches)
			}
			if err := o.watcher.Add(filepath.Join(o.cfg.Local.Root, p)); err != nil {
				return err
			}
			o.watched[p] = index.Fingerprint{Device: uint64(st.Dev), Inode: st.Ino}
			o.set(func(s *Status) { s.Watches = len(o.watched) })
		}
		f, err := o.root.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			if err := o.pace(ctx); err != nil {
				return err
			}
			entries, readErr := f.ReadDir(128)
			for _, e := range entries {
				child := e.Name()
				if p != "." {
					child = p + "/" + child
				}
				if o.matcher.Match(child, e.IsDir()) {
					continue
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	// Rebuild watch membership on a full reconciliation. Closing a stale watch
	// does not read its old path and prevents moved-out directories being tracked.
	if len(roots) == 1 && roots[0] == "." {
		for p := range o.watched {
			_ = o.watcher.Remove(filepath.Join(o.cfg.Local.Root, p))
			delete(o.watched, p)
		}
	}
	for _, rel := range roots {
		if err := walk(rel, 0); err != nil {
			return err
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if err := checkRoot(); err != nil {
		return err
	}
	// Only a complete walk can label unseen records as missing. Process one page
	// at a time, preserving pause/dirty metadata and avoiding a whole-tree slice.
	missingUpdates := make([]index.Record, 0, 128)
	flushMissing := func() error {
		if len(missingUpdates) == 0 {
			return nil
		}
		// Reconfirm absence even if the previous index record was already
		// missing: a create/delete pair can coalesce away while a journal head
		// advances independently. Restart reconciliation must cover the same
		// window. This runs only after a complete requested metadata scan;
		// it adds no polling or filesystem reads.
		err := o.db.Put(missingUpdates, true)
		missingUpdates = missingUpdates[:0]
		return err
	}
	for _, rel := range roots {
		after := ""
		prefix := ""
		var exact []index.Record
		if rel != "." {
			prefix = rel + "/"
			r, ok, err := o.db.Get(rel)
			if err != nil {
				return err
			}
			if ok {
				exact = []index.Record{r}
			}
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			page, err := o.db.PagePrefix(prefix, after, 128)
			if err != nil {
				return err
			}
			if len(exact) > 0 {
				page = append(exact, page...)
				exact = nil
			}
			if len(page) == 0 {
				break
			}

			for _, r := range page {
				after = r.Path
				if rel != "." && r.Path != rel && !strings.HasPrefix(r.Path, rel+"/") {
					continue
				}
				if r.Scan == id {
					continue
				}
				isDir := os.FileMode(r.Fingerprint.Mode).IsDir()
				excluded := o.matcher.Match(r.Path, isDir)
				ancestor := r.Path
				for !excluded {
					slash := strings.LastIndexByte(ancestor, '/')
					if slash < 0 {
						break
					}
					ancestor = ancestor[:slash]
					parent, ok, err := o.db.Get(ancestor)
					if err != nil {
						return err
					}
					if ok && parent.Excluded {
						excluded = true
					}
				}
				if excluded {
					r.Excluded = true
				} else if !r.Excluded {
					r.Missing = true
				}
				if !r.Missing && !r.Excluded {
					continue
				}
				missingUpdates = append(missingUpdates, r)
				if len(missingUpdates) == cap(missingUpdates) {
					if err := flushMissing(); err != nil {
						return err
					}
				}
				if _, watched := o.watched[r.Path]; watched && (r.Missing || r.Excluded) {
					_ = o.watcher.Remove(filepath.Join(o.cfg.Local.Root, r.Path))
					delete(o.watched, r.Path)
				}
			}

		}
	}
	if err := flushMissing(); err != nil {
		return err
	}
	o.set(func(s *Status) { s.Watches = len(o.watched) })
	return nil
}

// ScanOnce creates a metadata snapshot and exits. No hashing/network work occurs.
// As with Run, errors leave the recovery marker set and cannot imply deletions.
func (o *Observer) ScanOnce(ctx context.Context) error {
	defer o.root.Close()
	defer o.watcher.Close()
	if err := o.db.SetRecovery(true); err != nil {
		return err
	}
	if err := o.scan(ctx, ".", false); err != nil {
		return o.fail(err)
	}
	o.set(func(s *Status) { s.Ready = true })
	return nil
}
