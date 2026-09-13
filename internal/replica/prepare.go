package replica

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"nas-sync/internal/content"
	"nas-sync/internal/delta"
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
	"nas-sync/internal/stage"
)

// PrepareUpload compares one explicit bounded path selection, captures only push
// candidates and durably prepares one diff outbox. Other decisions are returned
// for change replay/conflict handling. It never sends payload or publishes files.
// Callers serialize replica work and order parent/child operations separately.
// Budgeted replicas reserve snapshot/wire capacity after durable preparation and
// before writing content; the automatic worker requires that budget to be present.
// On preparation failure, AbortUploadPreparation must finish before new work.
func PrepareUpload(ctx context.Context, db *index.DB, root *content.Root, c *journal.Coordinator, store *stage.Store, remote ComparisonRemote, paths []string, namespace string, o PushOptions) (*index.Upload, []Comparison, error) {
	if db == nil || root == nil || c == nil || store == nil || remote == nil || !manifest.ValidID(namespace) || !o.Writes || o.Gate == nil || len(paths) < 1 || len(paths) > journal.MaxEntries {
		return nil, nil, fmt.Errorf("explicit bounded upload preparation required")
	}
	if o.MaxFileBytes < 1 || o.MaxFileBytes > 8<<30 || o.MaxBatchBytes < o.MaxFileBytes || o.MaxBatchBytes > 8<<30 || o.ReadBytesPerSecond < 1024 || o.ReadBytesPerSecond > 1<<30 {
		return nil, nil, fmt.Errorf("bounded upload preparation limits required")
	}
	if err := c.CheckStore(store); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, contentTimeout(o.MaxBatchBytes, o.ReadBytesPerSecond))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if u, err := db.PendingUpload(); err != nil || u != nil {
		return nil, nil, errors.Join(index.ErrStale, err)
	}
	if p, err := db.PendingUploadPreparation(); err != nil || p != nil {
		return nil, nil, errors.Join(index.ErrStale, err)
	}
	// Bound aggregate source reads before comparing any content. Compare applies
	// exclusions and verifies path types before opening each selected source.
	seen := make(map[string]bool, len(paths))
	var total int64
	for _, path := range paths {
		if seen[path] {
			return nil, nil, fmt.Errorf("duplicate upload path")
		}
		seen[path] = true
		r, ok, err := db.Get(path)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, index.ErrStale
		}
		if !r.Missing && os.FileMode(r.Fingerprint.Mode).IsRegular() {
			if r.Fingerprint.Size < 0 || r.Fingerprint.Size > o.MaxFileBytes {
				return nil, nil, &localPathError{path, r.Generation, fmt.Errorf("%w: selected file exceeds content limit", ErrAttention)}
			}
			if r.Fingerprint.Size > o.MaxBatchBytes-total {
				// Observation can grow a file after selection. Start a fresh
				// bounded selection rather than permanently halting the worker.
				return nil, nil, fmt.Errorf("%w: selected batch grew beyond content limit", index.ErrStale)
			}
			total += r.Fingerprint.Size
		}
	}
	operation, err := hash.RandomDigest()
	if err != nil {
		return nil, nil, err
	}
	client, err := db.ClientID()
	if err != nil {
		return nil, nil, err
	}
	p := index.UploadPreparation{Namespace: namespace, ID: operation.Hex(), Client: client}
	results := make([]Comparison, len(paths))
	selected := make([]Comparison, 0, len(paths))
	bases := make([]*journal.Version, 0, len(paths))
	for i, path := range paths {
		comparison, err := Compare(ctx, db, root, c, remote, path, ComparisonOptions{Namespace: namespace, Exclusions: o.Exclusions, MaxFileBytes: o.MaxFileBytes, ReadBytesPerSecond: o.ReadBytesPerSecond, Gate: o.Gate})
		results[i] = comparison
		if err != nil {
			return nil, results, &localPathError{path, comparison.Generation, err}
		}
		if comparison.Decision.Action != diff.Push {
			continue
		}
		if comparison.Local == nil {
			return nil, results, fmt.Errorf("push requires a candidate")
		}
		r, ok, err := db.Get(path)
		if err != nil {
			return nil, results, err
		}
		if !ok || r.Generation != comparison.Generation {
			return nil, results, index.ErrStale
		}
		base, pending, err := c.Head(path)
		if err != nil {
			return nil, results, err
		}
		if pending || (base == nil) != (comparison.Base == nil) {
			return nil, results, index.ErrStale
		}
		expected := ""
		if base != nil {
			expected = base.ID
			if base.ID != comparison.Base.Content.ID {
				return nil, results, index.ErrStale
			}
			if !base.Tombstone && !comparison.Local.Content.Tombstone && base.Directory != comparison.Local.Content.Directory {
				return nil, results, &localPathError{path, comparison.Generation, fmt.Errorf("%w: type change requires prior explicit deletion", ErrAttention)}
			}
		}
		p.Sources = append(p.Sources, index.UploadSource{Path: path, Expected: expected, ID: comparison.Local.Content.ID, Generation: comparison.Generation, Fingerprint: r.Fingerprint, Missing: r.Missing})
		selected = append(selected, comparison)
		bases = append(bases, base)
	}
	if len(p.Sources) == 0 {
		return nil, results, nil
	}
	if err := p.Validate(); err != nil {
		return nil, results, err
	}
	check := func(i int) (err error) {
		defer func() {
			if err != nil {
				err = &localPathError{p.Sources[i].Path, p.Sources[i].Generation, err}
			}
		}()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := o.Gate(ctx); err != nil {
			return err
		}
		paused, err := db.Paused()
		if err != nil {
			return err
		}
		if paused {
			return fmt.Errorf("upload preparation paused")
		}
		s := p.Sources[i]
		state, err := db.PathState(s.Path)
		if err != nil {
			return err
		}
		if state.Paused || state.Conflict != nil {
			return fmt.Errorf("upload source paused or conflicted")
		}
		r, ok, err := db.Get(s.Path)
		if err != nil {
			return err
		}
		if !ok || !r.Dirty || r.Excluded || r.Generation != s.Generation || r.Fingerprint != s.Fingerprint || r.Missing != s.Missing {
			return index.ErrStale
		}
		base, pending, err := c.Head(s.Path)
		if err != nil {
			return err
		}
		if pending || !reflect.DeepEqual(base, bases[i]) {
			return index.ErrStale
		}
		fp, missing, err := root.Metadata(s.Path)
		if err != nil {
			return err
		}
		if missing != s.Missing || (!missing && !content.SameObservedContent(fp, s.Fingerprint)) {
			return content.ErrChanged
		}
		return nil
	}
	u := index.Upload{Namespace: namespace, Proposal: journal.Proposal{ID: p.ID, Client: p.Client}, Generations: make([]uint64, len(p.Sources)), DeltaBytes: make([]int64, len(p.Sources)), DeltaDigests: make([]hash.Digest, len(p.Sources))}
	err = db.BuildUpload(ctx, p, func() error {
		ids := make([]string, len(p.Sources))
		for i, s := range p.Sources {
			if err := check(i); err != nil {
				return err
			}
			if err := store.RequireUploadVacant(s.ID); err != nil {
				return err
			}
			ids[i] = s.ID
		}
		return c.CheckUnusedVersions(ids)
	}, func() (index.Upload, error) {
		if err := c.ReserveUploadCache(uploadBudgetProposal(p)); err != nil {
			return u, err
		}
		for i, s := range p.Sources {
			if err := check(i); err != nil {
				return u, err
			}
			m := selected[i].Local.Content
			v := journal.Version{ID: s.ID, Size: m.Size, Directory: m.Directory, Tombstone: m.Tombstone}
			if !m.Directory && !m.Tombstone {
				snapshot, err := root.SnapshotAs(ctx, s.Path, s.Fingerprint, store, s.ID, o.ReadBytesPerSecond)
				if err != nil {
					return u, &localPathError{s.Path, s.Generation, err}
				}
				if !reflect.DeepEqual(snapshot.Manifest, selected[i].Local) {
					return u, &localPathError{s.Path, s.Generation, content.ErrChanged}
				}
				v = snapshot.Version
				if err := check(i); err != nil {
					return u, err
				}
				sig, err := uploadBaseSignature(ctx, store, bases[i], o.ReadBytesPerSecond)
				if err != nil {
					return u, err
				}
				info, _, err := EncodeUpload(ctx, store, v, sig, o.ReadBytesPerSecond)
				if err != nil {
					return u, err
				}
				u.DeltaBytes[i], u.DeltaDigests[i] = info.Size, info.Digest
			}
			u.Generations[i] = s.Generation
			u.Proposal.Entries = append(u.Proposal.Entries, journal.Entry{Path: s.Path, Expected: s.Expected, Next: v})
		}
		for i := range p.Sources {
			if err := check(i); err != nil {
				return u, err
			}
		}
		return u, nil
	})
	if err != nil {
		return nil, results, err
	}
	return &u, results, nil
}

func uploadBaseSignature(ctx context.Context, store *stage.Store, base *journal.Version, rate int64) (*delta.Signature, error) {
	if base == nil || base.Tombstone {
		return delta.Build(ctx, bytes.NewReader(nil), 0, 65536)
	}
	if base.Directory {
		return nil, fmt.Errorf("directory cannot supply file blocks")
	}
	f, err := store.OpenReady(base.ID)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sig, err := delta.Build(ctx, &pacedReader{f, newPacer(ctx, rate)}, base.Size, 65536)
	if err != nil {
		return nil, err
	}
	if sig.Digest != base.Digest {
		return nil, fmt.Errorf("retained upload base differs")
	}
	return sig, nil
}

// AbortUploadPreparation is explicit local-only recovery for an untransmitted
// preparation. It never removes an outbox or committed version. Cleanup may be
// repeated after restart; dirty generations remain queued for fresh comparison.
func AbortUploadPreparation(ctx context.Context, db *index.DB, c *journal.Coordinator, store *stage.Store, namespace string) error {
	if db == nil || c == nil || store == nil || !manifest.ValidID(namespace) {
		return fmt.Errorf("upload recovery dependencies required")
	}
	if err := c.CheckStore(store); err != nil {
		return err
	}
	if err := c.BindReplicaOrigin(namespace); err != nil {
		return err
	}
	return db.AbortUploadPreparation(func(p index.UploadPreparation) error {
		if p.Namespace != namespace {
			return journal.ErrIdentity
		}
		ids := make([]string, len(p.Sources))
		for i, s := range p.Sources {
			ids[i] = s.ID
		}
		if err := c.CheckUnusedVersions(ids); err != nil {
			return err
		}
		return c.AbortUploadCache(uploadBudgetProposal(p), func() error {
			for _, s := range p.Sources {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := store.AbortUncommittedUpload(s.ID); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func uploadBudgetProposal(p index.UploadPreparation) journal.Proposal {
	proposal := journal.Proposal{ID: p.ID, Client: p.Client}
	for _, s := range p.Sources {
		v := journal.Version{ID: s.ID, Tombstone: s.Missing, Directory: !s.Missing && os.FileMode(s.Fingerprint.Mode).IsDir()}
		if !v.Tombstone && !v.Directory {
			v.Size, v.Digest = s.Fingerprint.Size, hash.SumBytes(nil)
		}
		proposal.Entries = append(proposal.Entries, journal.Entry{Path: s.Path, Expected: s.Expected, Next: v})
	}
	return proposal
}
