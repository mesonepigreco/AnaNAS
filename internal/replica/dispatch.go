package replica

import (
	"context"
	"fmt"
	"io"
	"reflect"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/exclude"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

type UploadRemote interface {
	OperationReader
	UploadState(context.Context, string) (uint64, bool, error)
	CommitUpload(context.Context, string, uint64, journal.Proposal, []int64, []io.Reader) (journal.Record, error)
	RecoverUpload(context.Context, string, uint64, journal.Proposal) (*journal.Record, error)
}

type PushOptions struct {
	Writes                                          bool
	Exclusions                                      []string
	MaxFileBytes, MaxBatchBytes, ReadBytesPerSecond int64
	Gate                                            func(context.Context) error
}

type Pusher struct {
	db         *index.DB
	store      *stage.Store
	remote     UploadRemote
	opts       PushOptions
	exclusions *exclude.Matcher
	active     chan struct{}
}

func NewPusher(db *index.DB, store *stage.Store, remote UploadRemote, o PushOptions) (*Pusher, error) {
	if db == nil || store == nil || remote == nil || o.Gate == nil {
		return nil, fmt.Errorf("upload dependencies and policy gate required")
	}
	if o.MaxFileBytes < 1 || o.MaxFileBytes > 8<<30 || o.MaxBatchBytes < o.MaxFileBytes || o.MaxBatchBytes > 8<<30 || o.ReadBytesPerSecond < 1024 || o.ReadBytesPerSecond > 1<<30 {
		return nil, fmt.Errorf("bounded upload resource limits required")
	}
	m, err := changefeed.Patterns(o.Exclusions)
	if err != nil {
		return nil, err
	}
	return &Pusher{db: db, store: store, remote: remote, opts: o, exclusions: m, active: make(chan struct{}, 1)}, nil
}

func (p *Pusher) checkPolicy(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.opts.Gate(ctx); err != nil {
		return err
	}
	paused, err := p.db.Paused()
	if err != nil {
		return err
	}
	if paused {
		return fmt.Errorf("upload paused")
	}
	return nil
}

func (p *Pusher) check(ctx context.Context, u *index.Upload) error {
	if err := p.checkPolicy(ctx); err != nil {
		return err
	}
	for i, e := range u.Proposal.Entries {
		state, err := p.db.PathState(e.Path)
		if err != nil {
			return err
		}
		if state.Paused || state.Conflict != nil {
			return fmt.Errorf("upload path paused or conflicted")
		}
		r, ok, err := p.db.Get(e.Path)
		if err != nil {
			return err
		}
		if !ok || r.Excluded || r.Generation < u.Generations[i] {
			return index.ErrStale
		}
	}
	return nil
}

func committedUpload(u *index.Upload) *journal.Record {
	return &journal.Record{Proposal: u.Proposal, Sequence: u.Receipt.Sequence, Epoch: u.Receipt.Epoch, Committed: true}
}

// Dispatch handles exactly one durable outbox operation. It queries remote
// outcome before opening payload spools, recovers prepared publication without
// re-uploading, and stores only an exact committed receipt. It does not clear
// local bases, build an outbox, retry internally or run an automatic loop.
func (p *Pusher) Dispatch(ctx context.Context) (*journal.Record, error) {
	if !p.opts.Writes {
		return nil, fmt.Errorf("upload dispatch disabled")
	}
	select {
	case p.active <- struct{}{}:
		defer func() { <-p.active }()
	default:
		return nil, fmt.Errorf("upload dispatcher busy")
	}
	ctx, cancel := context.WithTimeout(ctx, contentTimeout(p.opts.MaxBatchBytes, p.opts.ReadBytesPerSecond))
	defer cancel()
	u, err := p.db.PendingUpload()
	if err != nil || u == nil {
		return nil, err
	}
	var total int64
	for _, e := range u.Proposal.Entries {
		if p.exclusions.Match(e.Path, e.Next.Directory || e.Next.Tombstone) {
			return nil, fmt.Errorf("upload path excluded")
		}
		if e.Next.Size > p.opts.MaxFileBytes || e.Next.Size > p.opts.MaxBatchBytes-total {
			return nil, fmt.Errorf("upload exceeds configured content limits")
		}
		total += e.Next.Size
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if u.Receipt != nil {
		return committedUpload(u), nil
	}
	if err := p.check(ctx, u); err != nil {
		return nil, err
	}
	prior, err := p.remote.Operation(ctx, u.Namespace, u.Proposal.ID)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		if !reflect.DeepEqual(prior.Proposal, u.Proposal) {
			return nil, journal.ErrIdentity
		}
		if prior.Committed {
			if err := p.db.RecordUploadCommit(u.Namespace, *prior); err != nil {
				return nil, err
			}
			return prior, nil
		}
	}
	if err := p.check(ctx, u); err != nil {
		return nil, err
	}
	epoch, writes, err := p.remote.UploadState(ctx, u.Namespace)
	if err != nil {
		return nil, err
	}
	if !writes || epoch == 0 {
		return nil, fmt.Errorf("remote upload gate is closed")
	}
	if prior != nil {
		if err := p.check(ctx, u); err != nil {
			return nil, err
		}
		record, err := p.remote.RecoverUpload(ctx, u.Namespace, epoch, u.Proposal)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, journal.ErrPending
		}
		if err := p.db.RecordUploadCommit(u.Namespace, *record); err != nil {
			return nil, err
		}
		return record, nil
	}
	streams := make([]io.Reader, len(u.Proposal.Entries))
	opened := make([]io.Closer, 0, len(streams))
	defer func() {
		cancel()
		for _, stream := range opened {
			stream.Close()
		}
	}()
	for i, e := range u.Proposal.Entries {
		if e.Next.Directory || e.Next.Tombstone {
			continue
		}
		// Check global policy between spools. Full path validation runs again
		// before transmission, avoiding a quadratic set of index reads here.
		if err := p.checkPolicy(ctx); err != nil {
			return nil, err
		}
		stream, err := OpenUploadWire(ctx, p.store, e.Next.ID, stage.WireInfo{Size: u.DeltaBytes[i], Digest: u.DeltaDigests[i]}, p.opts.ReadBytesPerSecond)
		if err != nil {
			return nil, fmt.Errorf("upload spool for %q: %w", e.Path, err)
		}
		streams[i] = stream
		opened = append(opened, stream)
	}
	current, err := p.db.PendingUpload()
	if err != nil {
		return nil, err
	}
	if current == nil || !reflect.DeepEqual(current.Proposal, u.Proposal) || !reflect.DeepEqual(current.DeltaDigests, u.DeltaDigests) || !reflect.DeepEqual(current.DeltaBytes, u.DeltaBytes) || current.Namespace != u.Namespace {
		return nil, index.ErrStale
	}
	if current.Receipt != nil {
		return committedUpload(current), nil
	}
	if err := p.check(ctx, u); err != nil {
		return nil, err
	}
	record, err := p.remote.CommitUpload(ctx, u.Namespace, epoch, u.Proposal, u.DeltaBytes, streams)
	if err != nil {
		return nil, err
	}
	if err := p.db.RecordUploadCommit(u.Namespace, record); err != nil {
		return nil, err
	}
	return &record, nil
}
