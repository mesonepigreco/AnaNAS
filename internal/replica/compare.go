package replica

import (
	"context"
	"fmt"
	"os"
	"reflect"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/content"
	"nas-sync/internal/delta"
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

type ComparisonRemote interface {
	CompareHead(context.Context, string, string) (*journal.Version, bool, error)
	CompareSignature(context.Context, string, string, journal.Version) (*delta.Signature, error)
}

type ComparisonOptions struct {
	Namespace                        string
	Exclusions                       []string
	MaxFileBytes, ReadBytesPerSecond int64
	Gate                             func(context.Context) error
}

type Comparison struct {
	Decision            diff.Decision
	Generation          uint64
	Base, Local, Remote *manifest.Manifest
	// Cleared means only that the observed generation matches the acknowledged
	// local base. It does not assert the remote head is currently unchanged.
	Cleared bool
}

// Compare handles one observed dirty path, with no worker, queue or retry loop.
// Local content equal to the acknowledged base clears that exact generation
// without contacting the remote or creating snapshots. Otherwise authenticated
// head/signature metadata drives a three-way decision. It never uploads,
// publishes, deletes or records a conflict before its candidates are retained.
func Compare(ctx context.Context, db *index.DB, root *content.Root, c *journal.Coordinator, remote ComparisonRemote, path string, o ComparisonOptions) (Comparison, error) {
	var result Comparison
	if db == nil || root == nil || c == nil || remote == nil || !manifest.ValidID(o.Namespace) || o.Gate == nil || o.MaxFileBytes < 1 || o.MaxFileBytes > 8<<30 || o.ReadBytesPerSecond < 1024 || o.ReadBytesPerSecond > 1<<30 {
		return result, fmt.Errorf("bounded comparison dependencies and namespace required")
	}
	matcher, err := changefeed.Patterns(o.Exclusions)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, contentTimeout(o.MaxFileBytes, o.ReadBytesPerSecond))
	defer cancel()
	observed, ok, err := db.Get(path)
	if err != nil {
		return result, err
	}
	if !ok || !observed.Dirty || observed.Excluded || matcher.Match(path, observed.Missing || os.FileMode(observed.Fingerprint.Mode).IsDir()) {
		return result, index.ErrStale
	}
	result.Generation = observed.Generation
	base, err := db.Base(path)
	if err != nil {
		return result, err
	}
	result.Base = base
	if base != nil && (base.BlockSize != 65536 || base.Content.Size > o.MaxFileBytes) {
		return result, fmt.Errorf("%w: base exceeds comparison limits", ErrAttention)
	}
	if namespace, err := db.ReplicaNamespace(); err != nil {
		return result, err
	} else if namespace != "" && namespace != o.Namespace {
		return result, fmt.Errorf("comparison target differs from index")
	}
	if err := c.BindReplicaOrigin(o.Namespace); err != nil {
		return result, err
	}
	acknowledged, pending, err := c.Head(path)
	if err != nil {
		return result, err
	}
	if pending {
		return result, journal.ErrPending
	}
	if (base == nil) != (acknowledged == nil) {
		return result, fmt.Errorf("%w: local base and replica journal disagree", journal.ErrIdentity)
	}
	if base != nil && (acknowledged.ID != base.Content.ID || acknowledged.Size != base.Content.Size || acknowledged.Directory != base.Content.Directory || acknowledged.Tombstone != base.Content.Tombstone) {
		return result, fmt.Errorf("%w: local base differs from replica journal", journal.ErrIdentity)
	}
	check := func(ctx context.Context) error {
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
			return fmt.Errorf("comparison paused")
		}
		state, err := db.PathState(path)
		if err != nil {
			return err
		}
		if state.Paused || state.Conflict != nil {
			return fmt.Errorf("comparison path paused or conflicted")
		}
		current, ok, err := db.Get(path)
		if err != nil {
			return err
		}
		if !ok || current.Generation != observed.Generation || current.Excluded || !current.Dirty || current.Fingerprint != observed.Fingerprint || current.Missing != observed.Missing {
			return index.ErrStale
		}
		currentBase, err := db.Base(path)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(currentBase, base) {
			return index.ErrStale
		}
		head, pending, err := c.Head(path)
		if err != nil {
			return err
		}
		if pending || !reflect.DeepEqual(head, acknowledged) {
			return index.ErrStale
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return result, err
	}
	verify := func() error {
		fp, missing, err := root.Metadata(path)
		if err != nil {
			return err
		}
		if missing != observed.Missing || (!missing && !content.SameObservedContent(fp, observed.Fingerprint)) {
			return content.ErrChanged
		}
		return nil
	}
	if err := verify(); err != nil {
		return result, err
	}
	if observed.Missing || os.FileMode(observed.Fingerprint.Mode).IsDir() {
		// An unacknowledged missing path is absence, not delete intent.
		if !observed.Missing || base != nil {
			id, err := hash.RandomDigest()
			if err != nil {
				return result, err
			}
			result.Local = &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: id.Hex(), Directory: !observed.Missing, Tombstone: observed.Missing}}
		}
	} else {
		if observed.Fingerprint.Size < 0 || observed.Fingerprint.Size > o.MaxFileBytes {
			return result, fmt.Errorf("%w: local file exceeds comparison limit", ErrAttention)
		}
		result.Local, err = root.Hash(ctx, path, observed.Fingerprint, 65536, o.ReadBytesPerSecond)
		if err != nil {
			return result, err
		}
	}
	if err := check(ctx); err != nil {
		return result, err
	}
	if base != nil && result.Local != nil && diff.Plan(nil, &base.Content, &result.Local.Content).Action == diff.Noop {
		result.Local.Content.ID = base.Content.ID
		if err := verify(); err != nil {
			return result, err
		}
		result.Cleared, err = db.Acknowledge(path, base.Content.ID, observed.Generation, base)
		result.Decision = diff.Decision{Action: diff.Noop, Reason: "local content matches acknowledged base; no remote request needed"}
		return result, err
	}
	v, pending, err := remote.CompareHead(ctx, o.Namespace, path)
	if err != nil {
		return result, err
	}
	if pending {
		return result, journal.ErrPending
	}
	if v != nil {
		if err := journal.ValidateVersion(*v); err != nil {
			return result, err
		}
		if v.Size > o.MaxFileBytes {
			return result, fmt.Errorf("%w: remote file exceeds comparison limit", ErrAttention)
		}
		m := &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: v.ID, Size: v.Size, Directory: v.Directory, Tombstone: v.Tombstone}}
		if acknowledged != nil && v.ID == acknowledged.ID {
			if *v != *acknowledged {
				return result, journal.ErrIdentity
			}
			// The immutable ID and full description match the verified local
			// base, so its block manifest needs no remote rebuild or transfer.
			m = base
		} else if !v.Directory && !v.Tombstone {
			if err := check(ctx); err != nil {
				return result, err
			}
			sig, err := remote.CompareSignature(ctx, o.Namespace, path, *v)
			if err != nil {
				return result, err
			}
			if sig == nil {
				return result, fmt.Errorf("remote signature required")
			}
			if err := sig.Validate(); err != nil {
				return result, err
			}
			if sig.BlockSize != 65536 || sig.Size != v.Size || sig.Digest != v.Digest {
				return result, fmt.Errorf("remote signature differs from approved head")
			}
			m.Content.Blocks = make([]diff.Block, len(sig.Blocks))
			for i, b := range sig.Blocks {
				m.Content.Blocks[i] = diff.Block{Digest: b.Strong, Size: min(int64(65536), v.Size-int64(i)*65536)}
			}
		}
		if err := m.Validate(); err != nil {
			return result, err
		}
		result.Remote = m
	}
	if err := check(ctx); err != nil {
		return result, err
	}
	if err := verify(); err != nil {
		return result, err
	}
	var b, l, r *diff.Manifest
	if base != nil {
		b = &base.Content
	}
	if result.Local != nil {
		l = &result.Local.Content
	}
	if result.Remote != nil {
		r = &result.Remote.Content
	}
	result.Decision = diff.Plan(b, l, r)
	if b == nil && l == nil && r == nil {
		result.Cleared, err = db.AcknowledgeAbsent(path, observed.Generation)
		return result, err
	}
	// Equal remote content still needs authenticated change replay to establish
	// its journal provenance. This comparison does not acknowledge that new ID.
	return result, nil
}
