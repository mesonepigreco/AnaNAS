package journal

import (
	"context"
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/manifest"
)

var cachePrunedBucket = []byte("replica-pruned-snapshots-v1")
var cachePendingKey = []byte("replica-cache-removal-v1")

// CacheReport describes only proven committed PC-replica snapshots. Unknown
// files, publication backups, receipts and metadata are deliberately untouched.
type CacheReport struct {
	Scanned        int              `json:"scanned"`
	Candidates     int              `json:"candidates"`
	Removed        int              `json:"removed"`
	AllocatedBytes int64            `json:"allocatedBytes"`
	LogicalBytes   int64            `json:"logicalBytes"`
	Missing        int              `json:"missing"`
	Next           string           `json:"next,omitempty"`
	Details        []CacheCandidate `json:"-"`
}

type CacheCandidate struct {
	Version        string `json:"version"`
	Path           string `json:"path"`
	LogicalBytes   int64  `json:"logicalBytes"`
	AllocatedBytes int64  `json:"allocatedBytes"`
}

type CacheGuard func(context.Context, []Entry, func(map[string]bool) error) error

// MaintainReplicaCache runs under exclusive journal ownership and an index
// reference freeze. Only superseded, unpinned, path-bound file versions qualify.
// Metadata/operation identities remain immutable for retries and change cursors.
// It is NEVER available on a NAS helper, whose history may serve other clients.
func (c *Coordinator) MaintainReplicaCache(ctx context.Context, namespace, after string, limit int, remove bool, guard CacheGuard) (report CacheReport, err error) {
	if !manifest.ValidID(namespace) || (after != "" && !manifest.ValidID(after)) || limit < 1 || limit > 4096 || guard == nil {
		return report, fmt.Errorf("bounded replica maintenance and index guard required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return report, fmt.Errorf("coordinator closed")
	}
	var pending []Version
	check := func(tx *bolt.Tx) error {
		if err := c.checkEpoch(tx, c.epoch); err != nil {
			return err
		}
		u, err := readBudget(tx)
		if err != nil {
			return err
		}
		if u == nil || u.ReplicaNamespace != namespace || string(tx.Bucket(metaBucket).Get(replicaOriginKey)) != namespace {
			return fmt.Errorf("matching PC replica budget/origin required")
		}
		return nil
	}
	var candidates []Version
	var entries []Entry
	journalPins := map[string]bool{}
	if err := c.db.View(func(tx *bolt.Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		m := tx.Bucket(metaBucket)
		pinProposal := func(p Proposal) {
			for _, e := range p.Entries {
				journalPins[e.Expected] = true
				journalPins[e.Next.ID] = true
			}
		}
		if id := m.Get([]byte("pending")); id != nil {
			r, err := decode(tx.Bucket(operationsBucket).Get(id))
			if err != nil {
				return err
			}
			if r.Committed {
				return ErrIdentity
			}
			pinProposal(r.Proposal)
		}
		if p, err := readIntake(tx); err != nil {
			return err
		} else if p != nil {
			pinProposal(*p)
		} else if m.Get(externalKey) != nil {
			return ErrIdentity
		}
		if raw := m.Get(cachePendingKey); raw != nil {
			if len(raw) > MaxRecordBytes {
				return ErrIdentity
			}
			if err := json.Unmarshal(raw, &pending); err != nil {
				return err
			}
			if len(pending) < 1 || len(pending) > 128 {
				return ErrIdentity
			}
			if !remove {
				return ErrPending
			}
			candidates = pending
			for _, v := range candidates {
				stored, err := readVersion(tx, v.ID)
				if err != nil {
					return err
				}
				if stored.Version != v || stored.Path == "" || v.Directory || v.Tombstone || journalPins[v.ID] || string(tx.Bucket(headsBucket).Get([]byte(stored.Path))) == v.ID {
					return ErrIdentity
				}
				entries = append(entries, Entry{Path: stored.Path, Next: v})
			}
			return nil
		}
		cur := tx.Bucket(versionsBucket).Cursor()
		k, _ := cur.First()
		if after != "" {
			k, _ = cur.Seek([]byte(after))
			if string(k) == after {
				k, _ = cur.Next()
			}
		}
		for ; k != nil && report.Scanned < limit && len(candidates) < 128; k, _ = cur.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			report.Scanned++
			report.Next = string(k)
			v, err := readVersion(tx, string(k))
			if err != nil {
				return err
			}
			if v.Directory || v.Tombstone || v.Path == "" || journalPins[v.ID] || string(tx.Bucket(headsBucket).Get([]byte(v.Path))) == v.ID {
				continue
			}
			if b := tx.Bucket(cachePrunedBucket); b != nil && b.Get(k) != nil {
				continue
			}
			candidates = append(candidates, v.Version)
			entries = append(entries, Entry{Path: v.Path, Next: v.Version})
		}
		if k == nil {
			report.Next = ""
		}
		return nil
	}); err != nil {
		return report, err
	}
	if len(candidates) == 0 {
		return report, nil
	}
	err = guard(ctx, entries, func(pins map[string]bool) error {
		// Inspect every candidate before persisting intent. An absent object is
		// not proof of a prior owned removal and must never earn budget credit.
		var selected []Version
		paths := map[string]string{}
		for _, e := range entries {
			paths[e.Next.ID] = e.Path
		}
		for _, v := range candidates {
			if err := ctx.Err(); err != nil {
				return err
			}
			if pins[v.ID] {
				if pending != nil {
					return ErrIdentity
				}
				continue
			}
			allocated, present, err := c.store.ReplicaSnapshot(v.ID, v.Size, false)
			if err != nil {
				return err
			}
			if !present && pending == nil {
				report.Missing++
				continue
			}
			selected = append(selected, v)
			report.Candidates++
			report.AllocatedBytes += allocated
			report.LogicalBytes += v.Size
			report.Details = append(report.Details, CacheCandidate{Version: v.ID, Path: paths[v.ID], LogicalBytes: v.Size, AllocatedBytes: allocated})
		}
		if !remove || len(selected) == 0 {
			return nil
		}
		if err := c.db.View(func(tx *bolt.Tx) error {
			u, err := readBudget(tx)
			if err != nil {
				return err
			}
			var credit int64
			seen := map[string]bool{}
			for _, v := range selected {
				if seen[v.ID] {
					return ErrIdentity
				}
				seen[v.ID] = true
				if b := tx.Bucket(cachePrunedBucket); b != nil && b.Get([]byte(v.ID)) != nil {
					return ErrIdentity
				}
				credit += v.Size
			}
			if u == nil || credit > u.ReservedBytes {
				return ErrIdentity
			}
			return nil
		}); err != nil {
			return err
		}
		if pending == nil {
			raw, err := json.Marshal(selected)
			if err != nil {
				return err
			}
			if err := c.db.Update(func(tx *bolt.Tx) error {
				if err := check(tx); err != nil {
					return err
				}
				return tx.Bucket(metaBucket).Put(cachePendingKey, raw)
			}); err != nil {
				return err
			}
		}
		for _, v := range selected {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, _, err := c.store.ReplicaSnapshot(v.ID, v.Size, true); err != nil {
				return err
			}
		}
		if err := c.store.Sync(); err != nil {
			return err
		}
		if err := c.db.Update(func(tx *bolt.Tx) error {
			if err := check(tx); err != nil {
				return err
			}
			u, err := readBudget(tx)
			if err != nil {
				return err
			}
			b, err := tx.CreateBucketIfNotExists(cachePrunedBucket)
			if err != nil {
				return err
			}
			for _, v := range selected {
				if b.Get([]byte(v.ID)) != nil {
					return ErrIdentity
				}
				// Credit just the owned payload, never retained metadata or the
				// conservative publication/old-base allowances still in history.
				if u.ReservedBytes < v.Size {
					return ErrIdentity
				}
				u.ReservedBytes -= v.Size
				if err := b.Put([]byte(v.ID), []byte{1}); err != nil {
					return err
				}
			}
			if err := updateBudget(tx, u); err != nil {
				return err
			}
			return tx.Bucket(metaBucket).Delete(cachePendingKey)
		}); err != nil {
			return err
		}
		report.Removed = len(selected)
		return nil
	})
	return report, err
}

func (c *Coordinator) CacheRemovalPending() (bool, error) {
	var pending bool
	err := c.db.View(func(tx *bolt.Tx) error { pending = tx.Bucket(metaBucket).Get(cachePendingKey) != nil; return nil })
	return pending, err
}
