// Package nasobserve captures native NAS edits into the synchronization journal.
// It consumes coalesced metadata hints; it never polls or scans file contents at
// idle. Visible paths are not rewritten by capture.
package nasobserve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"

	"nas-sync/internal/exclude"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/publish"
	"nas-sync/internal/stage"
)

type Status struct {
	Compared  uint64 `json:"compared"`
	Imported  uint64 `json:"imported"`
	Attention uint64 `json:"attention"`
	LastError string `json:"lastError,omitempty"`
}

type Worker struct {
	db             *index.DB
	c              *journal.Coordinator
	p              *publish.Publisher
	s              *stage.Store
	client         string
	maxFile        int64
	ready          func() bool
	exclusions     *exclude.Matcher
	hints, changes chan struct{}
	mu             sync.Mutex
	status         Status
}

func New(db *index.DB, c *journal.Coordinator, p *publish.Publisher, s *stage.Store, maxFile int64, patterns []string, ready func() bool) (*Worker, error) {
	if db == nil || c == nil || p == nil || s == nil || ready == nil || maxFile < 1 || maxFile > 8<<30 {
		return nil, fmt.Errorf("native observation dependencies and file limit required")
	}
	if err := c.CheckStore(s); err != nil {
		return nil, err
	}
	u, err := c.PublicationBudgetUsage()
	if err != nil {
		return nil, err
	}
	if u == nil || u.ReplicaNamespace != "" {
		return nil, fmt.Errorf("native capture budget required")
	}
	client, err := db.ClientID()
	if err != nil {
		return nil, err
	}
	m, err := exclude.Compile(patterns)
	if err != nil {
		return nil, err
	}
	return &Worker{db: db, c: c, p: p, s: s, maxFile: maxFile, client: client, ready: ready, exclusions: m, hints: make(chan struct{}, 1), changes: make(chan struct{}, 1)}, nil
}

func (w *Worker) Notify() {
	select {
	case w.hints <- struct{}{}:
	default:
	}
}
func (w *Worker) Interrupt()               { w.Notify() }
func (w *Worker) Changes() <-chan struct{} { return w.changes }
func (w *Worker) Status() Status           { w.mu.Lock(); defer w.mu.Unlock(); return w.status }
func (w *Worker) update(f func(*Status)) {
	w.mu.Lock()
	f(&w.status)
	w.mu.Unlock()
	select {
	case w.changes <- struct{}{}:
	default:
	}
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.hints:
		}
		if !w.ready() {
			continue
		}
		if err := w.reconcile(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.update(func(s *Status) { s.LastError = err.Error(); s.Attention++ })
		}
	}
}

func (w *Worker) reconcile(ctx context.Context) error {
	// A crash after sealing but before committing resumes the retained bytes.
	// Never clear an observation here: the visible path may contain a later edit.
	prior, err := w.c.PendingExternal()
	if err != nil {
		return err
	}
	if prior != nil {
		_, err := w.c.ImportExternal(ctx, w.c.Epoch(), *prior, func(ctx context.Context, resume bool) (journal.Version, error) {
			return w.p.CaptureExternal(ctx, prior.Entries[0], nil, w.s, resume)
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if abort := w.c.AbortExternal(ctx, w.c.Epoch(), prior.ID); abort != nil {
				return errors.Join(err, abort)
			}
		} else {
			w.update(func(s *Status) { s.Imported++ })
		}
	}
	w.update(func(s *Status) { s.Attention = 0; s.LastError = "" })
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !w.ready() {
			return nil
		}
		page, err := w.db.DirtyPage(after, 32)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, r := range page {
			after = r.Path
			if err := w.capture(ctx, r); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				w.update(func(s *Status) { s.Attention++; s.LastError = fmt.Sprintf("%s: %v", r.Path, err) })
				// A pending publication/intake serializes all native work. Wait
				// for the next reconciliation hint rather than spinning on it.
				if errors.Is(err, journal.ErrPending) {
					return nil
				}
			}
		}
	}
}

func (w *Worker) capture(ctx context.Context, r index.Record) error {
	if r.Excluded || w.exclusions.Match(r.Path, os.FileMode(r.Fingerprint.Mode).IsDir()) {
		return nil
	}
	head, pending, err := w.c.Head(r.Path)
	if err != nil {
		return err
	}
	if pending {
		return journal.ErrPending
	}
	if r.Missing {
		if head == nil || head.Tombstone {
			_, err := w.db.ClearObserved(r)
			return err
		}
		if head.Directory {
			return fmt.Errorf("directory deletion requires children-first reconciliation")
		}
	}
	mode := os.FileMode(r.Fingerprint.Mode)
	if !mode.IsRegular() && !mode.IsDir() {
		return fmt.Errorf("special files are not synchronized")
	}
	if !r.Missing && mode.IsRegular() && (r.Fingerprint.Size < 0 || r.Fingerprint.Size > w.maxFile) {
		return fmt.Errorf("file exceeds configured capture limit")
	}
	if !r.Missing && head != nil && !head.Tombstone && head.Directory != mode.IsDir() {
		return fmt.Errorf("file/directory type replacement awaits reconciliation")
	}
	// A child must not precede its directory in the changefeed.
	for parent := path.Dir(r.Path); parent != "."; parent = path.Dir(parent) {
		h, pending, err := w.c.Head(parent)
		if err != nil {
			return err
		}
		if pending {
			return journal.ErrPending
		}
		if h == nil || !h.Directory {
			return fmt.Errorf("parent directory is not yet synchronized")
		}
	}
	if !r.Missing && mode.IsDir() && head != nil && head.Directory {
		match, err := w.p.MatchExternalDirectory(ctx, r.Path, r.Fingerprint, head.ID)
		if err != nil {
			return err
		}
		if match {
			_, err := w.db.ClearObserved(r)
			return err
		}
	}
	if !r.Missing && mode.IsRegular() && head != nil && !head.Tombstone && head.Size == r.Fingerprint.Size {
		d, err := w.p.HashExternal(ctx, r.Path, r.Fingerprint)
		if err != nil {
			return err
		}
		w.update(func(s *Status) { s.Compared++ })
		if d == head.Digest {
			_, err := w.db.ClearObserved(r)
			return err
		}
	}
	op, err := hash.RandomDigest()
	if err != nil {
		return err
	}
	version, err := hash.RandomDigest()
	if err != nil {
		return err
	}
	expected := ""
	if head != nil {
		expected = head.ID
	}
	p := journal.Proposal{ID: op.Hex(), Client: w.client, Entries: []journal.Entry{{Path: r.Path, Expected: expected, Next: journal.Version{ID: version.Hex(), Size: r.Fingerprint.Size, Digest: hash.SumBytes(nil)}}}}
	if r.Missing {
		p.Entries[0].Next = journal.Version{ID: version.Hex(), Tombstone: true}
	} else if mode.IsDir() {
		p.Entries[0].Next = journal.Version{ID: version.Hex(), Directory: true}
	}
	_, err = w.c.ImportExternal(ctx, w.c.Epoch(), p, func(ctx context.Context, resume bool) (journal.Version, error) {
		return w.p.CaptureExternal(ctx, p.Entries[0], &r.Fingerprint, w.s, resume)
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		prior, check := w.c.PendingExternal()
		if check != nil {
			return errors.Join(err, check)
		}
		if prior != nil && prior.ID == p.ID {
			return errors.Join(err, w.c.AbortExternal(ctx, w.c.Epoch(), p.ID))
		}
		return err
	}
	w.update(func(s *Status) { s.Imported++ })
	_, err = w.db.ClearObserved(r)
	return err
}
