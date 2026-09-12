// Package replica joins bounded transfer operations to a local native journal
// and publisher. It is not an automatic scheduler or a LAN authorization source.
package replica

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/delta"
	"nas-sync/internal/exclude"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

type Downloader interface {
	DownloadVersion(context.Context, string, journal.Version, *delta.Signature, io.ReaderAt, io.Writer) (delta.Stats, error)
}

type PullOptions struct {
	Namespace                                       string
	Writes                                          bool
	Exclusions                                      []string
	MaxFileBytes, MaxBatchBytes, ReadBytesPerSecond int64
	Gate                                            func(context.Context) error
}

type Puller struct {
	journal    *journal.Coordinator
	store      *stage.Store
	publisher  journal.Publisher
	remote     Downloader
	opts       PullOptions
	exclusions *exclude.Matcher
	active     chan struct{}
}

type PullResult struct {
	Record    journal.Record
	Transfers []delta.Stats
}

// NewPuller starts no workers. It verifies the pinned store and persists the
// remote namespace binding before any pull. Dependencies belong to the same
// private native replica state outside both roots. Writes defaults false;
// production still needs validated egress and completed cache retention.
func NewPuller(c *journal.Coordinator, store *stage.Store, publisher journal.Publisher, remote Downloader, o PullOptions) (*Puller, error) {
	if c == nil || store == nil || publisher == nil || remote == nil || o.Gate == nil {
		return nil, fmt.Errorf("replica dependencies and policy gate required")
	}
	if err := c.CheckStore(store); err != nil {
		return nil, err
	}
	if o.MaxFileBytes < 1 || o.MaxFileBytes > 8<<30 || o.MaxBatchBytes < o.MaxFileBytes || o.MaxBatchBytes > 8<<30 || o.ReadBytesPerSecond < 1024 || o.ReadBytesPerSecond > 1<<30 {
		return nil, fmt.Errorf("bounded replica resource limits required")
	}
	m, err := changefeed.Patterns(o.Exclusions)
	if err != nil {
		return nil, err
	}
	if err := c.BindReplicaOrigin(o.Namespace); err != nil {
		return nil, err
	}
	return &Puller{journal: c, store: store, publisher: publisher, remote: remote, opts: o, exclusions: m, active: make(chan struct{}, 1)}, nil
}

type gatedPublisher struct {
	inner journal.Publisher
	gate  func(context.Context) error
	want  journal.Proposal
}

func (p gatedPublisher) Publish(ctx context.Context, r journal.Record) error {
	if !reflect.DeepEqual(r.Proposal, p.want) {
		return journal.ErrIdentity
	}
	if err := p.gate(ctx); err != nil {
		return err
	}
	return p.inner.Publish(ctx, r)
}

// Pull applies one explicit, already selected proposal. The caller binds it to
// a validated remote namespace/feed and owns inbox/base acknowledgements. A
// mismatching local head or visible external edit fails without choosing a
// conflict winner. Identical retry recovers prepared publication before doing
// any more network work. Durable intake reserves budget before downloading and
// pins partial ownership for retry. It never fetches a page, polls, or retries by itself.
func (p *Puller) Pull(ctx context.Context, proposal journal.Proposal) (PullResult, error) {
	var result PullResult
	if !p.opts.Writes {
		return result, fmt.Errorf("replica writes disabled")
	}
	if err := journal.ValidateProposal(proposal); err != nil {
		return result, err
	}
	proposal.Entries = append([]journal.Entry(nil), proposal.Entries...)
	var total int64
	for _, e := range proposal.Entries {
		if p.exclusions.Match(e.Path, e.Next.Directory || e.Next.Tombstone) {
			return result, fmt.Errorf("replica path excluded")
		}
		if e.Next.Size > p.opts.MaxFileBytes || e.Next.Size > p.opts.MaxBatchBytes-total {
			return result, fmt.Errorf("replica batch exceeds content limit")
		}
		total += e.Next.Size
	}
	select {
	case p.active <- struct{}{}:
		defer func() { <-p.active }()
	default:
		return result, fmt.Errorf("replica is busy")
	}
	ctx, cancel := context.WithTimeout(ctx, contentTimeout(p.opts.MaxBatchBytes, p.opts.ReadBytesPerSecond))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := p.opts.Gate(ctx); err != nil {
		return result, err
	}
	prior, err := p.journal.Operation(proposal.ID)
	if err != nil {
		return result, err
	}
	publication := gatedPublisher{p.publisher, p.opts.Gate, proposal}
	if prior != nil {
		if !reflect.DeepEqual(prior.Proposal, proposal) {
			return result, journal.ErrIdentity
		}
		if prior.Committed {
			result.Record = *prior
			return result, nil
		}
		done, err := p.journal.Recover(ctx, p.journal.Epoch(), publication)
		if err != nil {
			return result, err
		}
		if done == nil {
			done, err = p.journal.Operation(proposal.ID)
			if err != nil {
				return result, err
			}
		}
		if done == nil || !done.Committed || !reflect.DeepEqual(done.Proposal, proposal) {
			return result, journal.ErrPending
		}
		result.Record = *done
		return result, nil
	}
	bases := make([]*journal.Version, len(proposal.Entries))
	for i, e := range proposal.Entries {
		v, pending, err := p.journal.Head(e.Path)
		if err != nil {
			return result, err
		}
		if pending {
			return result, journal.ErrPending
		}
		head := ""
		if v != nil {
			head = v.ID
		}
		if head != e.Expected {
			return result, journal.ErrConflict
		}
		if v != nil && v.Directory && p.exclusions.Match(e.Path, true) {
			return result, fmt.Errorf("replica directory excluded")
		}
		if v != nil && !v.Tombstone && !e.Next.Tombstone && v.Directory != e.Next.Directory {
			return result, fmt.Errorf("type change requires explicit deletion")
		}
		bases[i] = v
	}
	result.Transfers = make([]delta.Stats, len(proposal.Entries))
	result.Record, err = p.journal.StageAndCommit(ctx, p.journal.Epoch(), proposal, func(ctx context.Context, resume bool) error {
		for i, e := range proposal.Entries {
			if err := p.opts.Gate(ctx); err != nil {
				return err
			}
			if e.Next.Directory || e.Next.Tombstone {
				continue
			}
			if resume {
				if err := p.store.DiscardPartial(e.Next.ID); err != nil {
					return err
				}
			}
			result.Transfers[i], err = p.stage(ctx, e, bases[i])
			if err != nil {
				return err
			}
		}
		return p.opts.Gate(ctx)
	}, publication)
	return result, err
}

func (p *Puller) stage(ctx context.Context, e journal.Entry, base *journal.Version) (delta.Stats, error) {
	var stats delta.Stats
	pace := newPacer(ctx, p.opts.ReadBytesPerSecond)
	// An earlier failed batch may have retained this exact candidate. Verify it
	// locally before reuse; the ready suffix alone is not proof after restart.
	ready, err := p.store.OpenReady(e.Next.ID)
	if err == nil {
		sig, checkErr := delta.Build(ctx, &pacedReader{ready, pace}, e.Next.Size, 64*1024)
		closeErr := ready.Close()
		if checkErr != nil || closeErr != nil {
			return stats, errors.Join(checkErr, closeErr)
		}
		if sig.Digest != e.Next.Digest {
			return stats, fmt.Errorf("retained replica candidate differs")
		}
		return stats, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return stats, err
	}
	var source io.ReaderAt
	var sig *delta.Signature
	if base == nil || base.Tombstone {
		sig, err = delta.Build(ctx, bytes.NewReader(nil), 0, 64*1024)
	} else {
		if base.Directory || base.Size > p.opts.MaxFileBytes {
			return stats, fmt.Errorf("invalid replica file base")
		}
		f, openErr := p.store.OpenReady(base.ID)
		if openErr != nil {
			return stats, openErr
		}
		defer f.Close()
		sig, err = delta.Build(ctx, &pacedReader{f, pace}, base.Size, 64*1024)
		if err == nil && sig.Digest != base.Digest {
			return stats, fmt.Errorf("replica base digest differs")
		}
		source = &pacedReaderAt{f, pace}
	}
	if err != nil {
		return stats, err
	}
	err = p.store.ReceiveVerified(ctx, e.Next.ID, e.Next.Size, e.Next.Digest, func(out io.Writer) error {
		var err error
		stats, err = p.remote.DownloadVersion(ctx, e.Path, e.Next, sig, source, &pacedWriter{out, pace})
		return err
	})
	return stats, err
}
