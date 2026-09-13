package replica

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/content"
	"nas-sync/internal/diff"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
	"nas-sync/internal/stage"
)

var ErrSuspended = errors.New("synchronization suspended")
var ErrAttention = errors.New("synchronization needs reconciliation")

type WorkerRemote interface {
	UploadRemote
	ComparisonRemote
	Downloader
	ChangesPage(context.Context, string, string, uint64, int) (changefeed.Page, error)
	AcknowledgeChanges(context.Context, string, string, uint64, uint64) error
}

type WorkerOptions struct {
	Namespace string
	PushOptions
	// Ready requires completed local observation. The gate still supplies actual
	// LAN/egress authorization; Ready alone cannot authorize network access.
	Ready func() bool
	Task  func() func()
}

type WorkerStatus struct {
	Phase                string `json:"phase"`
	LastError            string `json:"lastError,omitempty"`
	CompletedUploads     uint64 `json:"completedUploads"`
	AppliedRemoteBatches uint64 `json:"appliedRemoteBatches"`
}

// Worker is the single owner of replica operations while Run is active. It has
// one coalesced wake slot and one bounded transient-error retry timer.
// Construction requires a replica cache budget. Production callers still need
// validated egress, retention and remote notification delivery. Deployed observation does
// not instantiate it. Conflicts retain their durable work and stop the pass.
type Worker struct {
	db                *index.DB
	root              *content.Root
	c                 *journal.Coordinator
	store             *stage.Store
	remote            WorkerRemote
	opts              WorkerOptions
	pusher            *Pusher
	puller            *Puller
	wake, changes     chan struct{}
	running           atomic.Bool
	priorityRequested atomic.Bool
	mu                sync.Mutex
	status            WorkerStatus
	cancel            context.CancelFunc
}

func NewWorker(db *index.DB, root *content.Root, c *journal.Coordinator, store *stage.Store, publisher journal.Publisher, remote WorkerRemote, o WorkerOptions) (*Worker, error) {
	if db == nil || root == nil || c == nil || store == nil || publisher == nil || remote == nil || !manifest.ValidID(o.Namespace) || !o.Writes || o.Gate == nil || o.Ready == nil {
		return nil, fmt.Errorf("explicit worker dependencies and authorization required")
	}
	o.Exclusions = append([]string(nil), o.Exclusions...)
	budget, err := c.PublicationBudgetUsage()
	if err != nil {
		return nil, err
	}
	if budget == nil || budget.ReplicaNamespace != o.Namespace {
		return nil, fmt.Errorf("automatic worker requires a matching replica cache budget")
	}
	w := &Worker{db: db, root: root, c: c, store: store, remote: remote, opts: o, wake: make(chan struct{}, 1), changes: make(chan struct{}, 1), status: WorkerStatus{Phase: "waiting"}}
	push := o.PushOptions
	push.Gate = w.check
	w.pusher, err = NewPusher(db, store, remote, push)
	if err != nil {
		return nil, err
	}
	w.puller, err = NewPuller(c, store, publisher, remote, PullOptions{Namespace: o.Namespace, Writes: o.Writes, Exclusions: o.Exclusions, MaxFileBytes: o.MaxFileBytes, MaxBatchBytes: o.MaxBatchBytes, ReadBytesPerSecond: o.ReadBytesPerSecond, Gate: w.check})
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Worker) Status() WorkerStatus     { w.mu.Lock(); defer w.mu.Unlock(); return w.status }
func (w *Worker) Changes() <-chan struct{} { return w.changes }
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// RequestSync gives durable confirmations priority after the current operation.
func (w *Worker) RequestSync() {
	w.priorityRequested.Store(true)
	w.Notify()
}

// Interrupt cancels active I/O and schedules a fresh policy check. Call after
// durable pause/resume changes or a route/availability change. Cancellation is
// cooperative; kernel filesystem calls can still take their own timeout.
func (w *Worker) Interrupt() {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	w.Notify()
}

func (w *Worker) update(change func(*WorkerStatus)) {
	w.mu.Lock()
	before := w.status
	change(&w.status)
	changed := before != w.status
	w.mu.Unlock()
	if changed {
		select {
		case w.changes <- struct{}{}:
		default:
		}
	}
}

func (w *Worker) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !w.opts.Ready() {
		return fmt.Errorf("%w: local observation is not ready", ErrSuspended)
	}
	paused, err := w.db.Paused()
	if err != nil {
		return err
	}
	if paused {
		return fmt.Errorf("%w: paused", ErrSuspended)
	}
	if err := w.opts.Gate(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrSuspended, err)
	}
	return nil
}

func (w *Worker) operationOptions() PushOptions {
	o := w.opts.PushOptions
	o.Gate = w.check
	return o
}

// Run drains durable work in bounded turns, then sleeps on the wake channel.
// Notifications during a pass collapse to one follow-up pass starting at the
// beginning of the dirty index. Recoverable failures use one 2–30 second timer;
// policy, conflict, identity and capacity failures never retry internally.
func (w *Worker) Run(ctx context.Context) error {
	if !w.running.CompareAndSwap(false, true) {
		return fmt.Errorf("replica worker already running")
	}
	defer w.running.Store(false)
	defer w.update(func(s *WorkerStatus) { s.Phase = "stopped" })
	w.Notify()
	var retry *time.Timer
	retryDelay := 2 * time.Second
	for {
		var retryC <-chan time.Time
		if retry != nil {
			retryC = retry.C
		}
		select {
		case <-ctx.Done():
			if retry != nil {
				retry.Stop()
			}
			return nil
		case <-w.wake:
			if retry != nil {
				retry.Stop()
				retry = nil
			}
		case <-retryC:
			retry = nil
		}
		workCtx, cancel := context.WithCancel(ctx)
		w.mu.Lock()
		w.cancel = cancel
		w.mu.Unlock()
		var endTask func()
		if w.opts.Ready() && w.opts.Task != nil {
			endTask = w.opts.Task()
		}
		w.update(func(s *WorkerStatus) { s.Phase = "working"; s.LastError = "" })
		pass := workerPass{fetch: true}
		err := w.db.ClearTransientIssues(workCtx)
		for more := true; more; {
			if err != nil {
				break
			}
			if err = w.check(workCtx); err != nil {
				break
			}
			more, err = w.turn(workCtx, &pass)
			if err != nil {
				break
			}
		}
		partial := err == nil && pass.retry == nil && pass.attention != nil
		if err == nil && pass.retry != nil {
			err = pass.retry
		}
		if err == nil {
			err = pass.attention
		}
		cancel()
		if endTask != nil {
			endTask()
		}
		w.mu.Lock()
		w.cancel = nil
		w.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		w.update(func(s *WorkerStatus) {
			s.Phase, s.LastError = "idle", ""
			if err != nil {
				s.Phase, s.LastError = "attention", err.Error()
				if errors.Is(err, ErrSuspended) || errors.Is(err, context.Canceled) {
					s.Phase = "suspended"
				} else if retryableWorkerError(err) {
					s.Phase = "retrying"
				} else if partial {
					s.Phase = "partial"
				}
			}
		})
		if retryableWorkerError(err) {
			retry = time.NewTimer(retryDelay)
			if retryDelay < 30*time.Second {
				retryDelay *= 2
				if retryDelay > 30*time.Second {
					retryDelay = 30 * time.Second
				}
			}
		} else if err == nil {
			retryDelay = 2 * time.Second
		}
	}
}

func retryableWorkerError(err error) bool {
	if err == nil || errors.Is(err, ErrSuspended) || errors.Is(err, ErrAttention) ||
		errors.Is(err, journal.ErrBudget) || errors.Is(err, journal.ErrConflict) ||
		errors.Is(err, journal.ErrIdentity) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, content.ErrChanged) || errors.Is(err, index.ErrStale) || errors.Is(err, journal.ErrPending) {
		return true
	}
	var remote interface{ Retryable() bool }
	if errors.As(err, &remote) {
		return remote.Retryable()
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		return true
	}
	// syscall.Errno implements net.Error even for ENOENT and EACCES. Merely
	// matching that interface must not turn permanent file errors into retries.
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

func (w *Worker) finishOutbox(ctx context.Context) error {
	u, err := w.db.PendingUpload()
	if err != nil {
		return err
	}
	if u == nil || u.Namespace != w.opts.Namespace {
		return index.ErrStale
	}
	if u.Receipt != nil {
		err = w.c.CheckUploadCompletion(u.Proposal, u.Namespace)
	} else {
		err = w.c.CheckUploadCache(u.Proposal, u.Namespace)
	}
	if err != nil {
		return err
	}
	if _, err := w.pusher.Dispatch(ctx); err != nil {
		return fmt.Errorf("send upload %s: %w", u.Proposal.ID, err)
	}
	if err := FinishUpload(ctx, w.db, w.c, w.store, w.opts.Namespace, w.operationOptions()); err != nil {
		return fmt.Errorf("complete upload %s: %w", u.Proposal.ID, err)
	}
	w.update(func(s *WorkerStatus) { s.CompletedUploads++ })
	return nil
}

type workerPass struct {
	retry         error
	requestCursor string
	requestsDone  bool
	attention     error
	cursor        string
	fetch         bool
}

func (w *Worker) selectNextLocal(pass *workerPass) ([]string, bool, error) {
	if w.priorityRequested.Swap(false) {
		pass.requestsDone, pass.requestCursor = false, ""
	}
	var paths []string
	var next string
	var err error
	requested := !pass.requestsDone
	if requested {
		var records []index.Record
		records, err = w.db.RequestedPage(pass.requestCursor, journal.MaxEntries)
		if err == nil {
			paths, next, err = w.selectRecords(records, pass.requestCursor)
		}
		pass.requestCursor = next
		if next == "" {
			pass.requestsDone = true
		}
	} else {
		paths, next, err = w.selectLocal(pass.cursor)
		pass.cursor = next
	}
	if errors.Is(err, ErrAttention) && next != "" {
		pass.attention, err = err, nil
	}
	return paths, requested || next != "", err
}

func (w *Worker) turn(ctx context.Context, pass *workerPass) (bool, error) {
	// No other replica operation may interleave with unfinished local uploads.
	if u, err := w.db.PendingUpload(); err != nil {
		return false, err
	} else if u != nil {
		pass.fetch = true
		return true, w.finishOutbox(ctx)
	}
	if p, err := w.db.PendingUploadPreparation(); err != nil {
		return false, err
	} else if p != nil {
		return true, AbortUploadPreparation(ctx, w.db, w.c, w.store, w.opts.Namespace)
	}
	// Process a saved inbox first. At most one 32-record page is fetched per
	// turn; local work gets a turn before fetching another full remote page.
	state, err := w.db.RemoteState()
	if err != nil {
		return false, err
	}
	if state.Acknowledged < state.Completed {
		if err := w.acknowledge(ctx, state); err != nil {
			return false, err
		}
		state, err = w.db.RemoteState()
		if err != nil {
			return false, err
		}
		if state.Acknowledged < state.Completed {
			return true, nil
		}
	}
	if state.Received != state.Completed {
		pass.fetch = true
	}
	if state.Received == state.Completed && pass.fetch {
		if err := w.check(ctx); err != nil {
			return false, err
		}
		page, err := w.remote.ChangesPage(ctx, state.Namespace, state.Policy, state.Received, journal.MaxPage)
		if err != nil {
			return false, err
		}
		if page.Namespace != w.opts.Namespace {
			return false, journal.ErrIdentity
		}
		if err := w.db.ReceiveRemotePage(page); err != nil {
			return false, err
		}
		pass.fetch = len(page.Batches) == journal.MaxPage
	}
	for i := 0; i < journal.MaxPage; i++ {
		batch, err := w.db.NextRemoteBatch()
		if err != nil {
			return false, err
		}
		if batch == nil {
			break
		}
		if err := w.applyRemote(ctx, batch); err != nil {
			return false, fmt.Errorf("apply NAS update %d: %w", batch.Sequence, err)
		}
	}
	state, err = w.db.RemoteState()
	if err != nil {
		return false, err
	}
	if err := w.acknowledge(ctx, state); err != nil {
		return false, err
	}
	beforeSelection := *pass
	paths, more, err := w.selectNextLocal(pass)
	if err != nil {
		return false, err
	}
	if len(paths) == 0 {
		return more || pass.fetch, nil
	}
	u, comparisons, err := PrepareUpload(ctx, w.db, w.root, w.c, w.store, w.remote, paths, w.opts.Namespace, w.operationOptions())
	if err != nil {
		deferred, cleanupErr := w.deferLocalFailure(ctx, err, pass)
		if cleanupErr != nil {
			return false, cleanupErr
		}
		if deferred {
			// Preparation sent nothing; revisit the other selected paths after
			// recording the failing path, without losing their cursor positions.
			pass.cursor, pass.requestCursor = beforeSelection.cursor, beforeSelection.requestCursor
			pass.requestsDone = beforeSelection.requestsDone
			return true, nil
		}
		return false, err
	}
	// Complete any prepared push before reporting other decisions. No conflict
	// record is invented without retaining its candidates.
	if u != nil {
		if err := w.finishOutbox(ctx); err != nil {
			return false, err
		}
		pass.fetch = true
	}
	for i, c := range comparisons {
		if c.Decision.Action != diff.Push && !c.Cleared {
			reason := fmt.Sprintf("Needs reconciliation: %s (%s)", paths[i], c.Decision.Action)
			if err := w.db.RecordSyncIssue(paths[i], c.Generation, reason, false); err != nil {
				return false, err
			}
			pass.attention = fmt.Errorf("%w: %s; other eligible files continue", ErrAttention, reason)
		}
	}
	return true, nil
}

func (w *Worker) acknowledge(ctx context.Context, state index.RemoteState) error {
	if state.Acknowledged == state.Completed {
		return nil
	}
	if err := w.check(ctx); err != nil {
		return err
	}
	through := state.Completed
	if through-state.Acknowledged > journal.MaxPage {
		through = state.Acknowledged + journal.MaxPage
	}
	if err := w.remote.AcknowledgeChanges(ctx, state.Namespace, state.Policy, state.Acknowledged, through); err != nil {
		return err
	}
	return w.db.RecordRemoteAcknowledged(state.Namespace, state.Policy, state.Acknowledged, through)
}

func (w *Worker) applyRemote(ctx context.Context, batch *changefeed.Batch) error {
	if err := w.check(ctx); err != nil {
		return err
	}
	for _, e := range batch.Entries {
		if w.pusher.exclusions.Match(e.Path, e.Next.Directory || e.Next.Tombstone) {
			return fmt.Errorf("%w: remote exclusion policy differs", ErrAttention)
		}
		state, err := w.db.PathState(e.Path)
		if err != nil {
			return err
		}
		if state.Paused || state.Conflict != nil {
			return fmt.Errorf("%w: remote path %s is paused or conflicted", ErrAttention, e.Path)
		}
	}
	// Own uploaded entries (and fully excluded batches) can complete from the
	// durable bases without downloading or touching visible paths.
	if err := w.db.CompleteRemoteBatch(batch.Sequence, batch.ID); err == nil {
		return nil
	} else if !errors.Is(err, index.ErrStale) {
		return err
	}
	if _, err := w.puller.Pull(ctx, batch.Proposal); err != nil {
		return err
	}
	if err := FinishDownload(ctx, w.db, w.c, w.store, w.opts.Namespace, w.operationOptions()); err != nil {
		return err
	}
	w.update(func(s *WorkerStatus) { s.AppliedRemoteBatches++ })
	return nil
}

func (w *Worker) selectLocal(after string) ([]string, string, error) {
	page, err := w.db.DirtyPage(after, journal.MaxEntries)
	if err != nil {
		return nil, "", err
	}
	return w.selectRecords(page, after)
}

func (w *Worker) selectRecords(page []index.Record, after string) ([]string, string, error) {
	if len(page) == 0 {
		return nil, "", nil
	}
	paths := make([]string, 0, len(page))
	var total int64
	var attention error
	next := after
	for _, r := range page {
		if reason := index.UnsupportedReason(r, w.opts.MaxFileBytes); reason != "" {
			if r.Missing {
				cleared, err := w.db.ClearUnsupportedAbsence(r.Path, r.Generation)
				if err != nil {
					return nil, "", err
				}
				if cleared {
					next = r.Path
					continue
				}
			}
			attention = fmt.Errorf("%w: %q: %s; other eligible files continue", ErrAttention, r.Path, reason)
			next = r.Path
			continue
		}
		issue, err := w.db.SyncIssue(r)
		if err != nil {
			return nil, "", err
		}
		if issue != nil {
			attention = fmt.Errorf("%w: %q: %s; other eligible files continue", ErrAttention, r.Path, issue.Reason)
			next = r.Path
			continue
		}
		state, err := w.db.PathState(r.Path)
		if err != nil {
			return nil, "", err
		}
		if state.Paused || state.Conflict != nil {
			next = r.Path
			continue
		}
		// Keep parent creation and children in successive proposals. Do not move
		// the cursor past an overlapping child before its parent is acknowledged.
		for _, p := range paths {
			if strings.HasPrefix(r.Path, p+"/") {
				return paths, next, attention
			}
		}
		if r.Missing {
			base, err := w.db.Base(r.Path)
			if err != nil {
				return nil, "", err
			}
			if base != nil && base.Content.Directory {
				children, err := w.db.PagePrefix(r.Path+"/", "", 1)
				if err != nil {
					return nil, "", err
				}
				if len(children) != 0 {
					attention = fmt.Errorf("%w: directory deletion %q requires child reconciliation; other eligible files continue", ErrAttention, r.Path)
					next = r.Path
					continue
				}
			}
		} else if os.FileMode(r.Fingerprint.Mode).IsRegular() {
			size := r.Fingerprint.Size
			if size > w.opts.MaxBatchBytes-total {
				break
			}
			total += size
		}
		paths = append(paths, r.Path)
		next = r.Path
	}
	return paths, next, attention
}
