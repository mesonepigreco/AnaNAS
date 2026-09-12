package journal

import (
	"encoding/json"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"
)

var ErrBudget = errors.New("publication cache budget exhausted")

var budgetKey = []byte("publication-budget-v1")
var reservationsBucket = []byte("publication-reservations-v1")

// PublicationBudget bounds logical payload reservations and history entry count
// for a native receiver or PC replica. It is not a filesystem quota: bbolt pages, filesystem
// overhead and external writers' disk use are outside its byte accounting.
type PublicationBudget struct {
	MaxBytes   int64 `json:"maxBytes"`
	MaxEntries int64 `json:"maxEntries"`
}

type BudgetUsage struct {
	PublicationBudget
	ReplicaNamespace string `json:"replicaNamespace,omitempty"`
	ReservedBytes    int64  `json:"reservedBytes"`
	Entries          int64  `json:"entries"`
}

// These conservative allowances are charged even for zero-byte objects. Entry
// count independently bounds retained records; allowances are not measurements
// or guarantees about physical database allocation.
const budgetEntryOverhead int64 = 64 << 10
const budgetOperationOverhead int64 = 128 << 10

func (b PublicationBudget) Validate() error {
	if b.MaxBytes < budgetEntryOverhead+budgetOperationOverhead || b.MaxBytes > 8<<40 || b.MaxEntries < 1 || b.MaxEntries > 1_000_000 {
		return fmt.Errorf("publication budget requires 192 KiB to 8 TiB and one to 1000000 entries")
	}
	return nil
}

func readBudget(tx *bolt.Tx) (*BudgetUsage, error) {
	raw := tx.Bucket(metaBucket).Get(budgetKey)
	if raw == nil {
		if tx.Bucket(reservationsBucket) != nil {
			return nil, fmt.Errorf("publication reservations lack a budget")
		}
		return nil, nil
	}
	var u BudgetUsage
	if len(raw) > 1024 {
		return nil, fmt.Errorf("oversized publication budget")
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	if err := u.PublicationBudget.Validate(); err != nil {
		return nil, err
	}
	if u.ReplicaNamespace != "" && (!manifest.ValidID(u.ReplicaNamespace) || string(tx.Bucket(metaBucket).Get(replicaOriginKey)) != u.ReplicaNamespace) {
		return nil, fmt.Errorf("cache budget replica origin differs")
	}
	if u.ReservedBytes < 0 || u.ReservedBytes > u.MaxBytes || u.Entries < 0 || u.Entries > u.MaxEntries || tx.Bucket(reservationsBucket) == nil {
		return nil, fmt.Errorf("invalid publication budget accounting")
	}
	return &u, nil
}

// ConfigurePublicationBudget initializes only an unused receiver journal with no
// payload. Reopening accepts identical limits or a monotonic capacity increase;
// it refuses decreases and mode changes. Open alone cannot disable an established
// budget. Charges survive commit, failure and restart. This mode covers receiver
// publication; PC spools require ConfigureReplicaBudget. Committed history
// reclamation is not yet implemented.
func (c *Coordinator) ConfigurePublicationBudget(b PublicationBudget) error {
	return c.configureBudget(b, "")
}

// ConfigureReplicaBudget enables shared accounting for downloaded publications
// and locally prepared snapshot/wire spools. Fresh state may contain index.db;
// existing unaccounted content/history is refused. The origin and mode persist.
func (c *Coordinator) ConfigureReplicaBudget(b PublicationBudget, namespace string) error {
	if !manifest.ValidID(namespace) {
		return fmt.Errorf("replica namespace required")
	}
	return c.configureBudget(b, namespace)
}

func (c *Coordinator) configureBudget(b PublicationBudget, namespace string) error {
	if err := b.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("coordinator closed")
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		prior, err := readBudget(tx)
		if err != nil {
			return err
		}
		if prior != nil {
			if prior.ReplicaNamespace != namespace {
				return fmt.Errorf("persisted publication budget mode differs")
			}
			if b.MaxBytes < prior.MaxBytes || b.MaxEntries < prior.MaxEntries {
				return fmt.Errorf("persisted publication budget cannot decrease")
			}
			if prior.PublicationBudget == b {
				return nil
			}
			// A monotonic increase preserves every reservation and its accounting.
			// It is safe on used journals because usage valid under the old limits
			// is necessarily valid under the new limits. Decreases remain refused.
			prior.PublicationBudget = b
			raw, err := json.Marshal(prior)
			if err != nil {
				return err
			}
			return tx.Bucket(metaBucket).Put(budgetKey, raw)
		}
		m := tx.Bucket(metaBucket)
		if origin := m.Get(replicaOriginKey); origin != nil && (namespace == "" || string(origin) != namespace) {
			return fmt.Errorf("publication-only budget cannot account for replica upload spools")
		}
		if m.Get([]byte("pending")) != nil || m.Get(intakeKey) != nil {
			return ErrPending
		}
		for _, name := range [][]byte{headsBucket, versionsBucket, operationsBucket, changesBucket, cursorsBucket} {
			if k, _ := tx.Bucket(name).Cursor().First(); k != nil {
				return fmt.Errorf("cannot budget previously used journal")
			}
		}
		if err := c.store.RequireBudgetBootstrap(namespace != ""); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(reservationsBucket); err != nil {
			return err
		}
		if namespace != "" {
			if err := m.Put(replicaOriginKey, []byte(namespace)); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(BudgetUsage{PublicationBudget: b, ReplicaNamespace: namespace})
		if err != nil {
			return err
		}
		return m.Put(budgetKey, raw)
	})
}

// PublicationBudgetUsage is an exact metadata lookup, with no file reads or
// enumeration. A nil result means this legacy/explicit fixture has no budget.
func (c *Coordinator) PublicationBudgetUsage() (*BudgetUsage, error) {
	var u *BudgetUsage
	err := c.db.View(func(tx *bolt.Tx) error {
		var err error
		u, err = readBudget(tx)
		return err
	})
	return u, err
}

// reservePublication runs in the same transaction as intake/preparation. A
// refusal rolls back all ownership changes and occurs before receiver/publisher
// callbacks. Exact retries have one charge. The ready candidate, publication
// copy and displaced old base are all conservatively charged; no credit is
// returned on commit, because retained history has not yet been reclaimed.
func reservePublication(tx *bolt.Tx, p Proposal) error {
	u, err := readBudget(tx)
	if err != nil || u == nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	digest := hash.SumBytes(raw)
	b := tx.Bucket(reservationsBucket)
	if prior := b.Get([]byte(p.ID)); prior != nil {
		if string(prior) != string(digest[:]) {
			return ErrIdentity
		}
		return nil
	}
	charge, err := publicationCharge(tx, p)
	if err != nil {
		return err
	}
	if charge > u.MaxBytes-u.ReservedBytes || int64(len(p.Entries)) > u.MaxEntries-u.Entries {
		return ErrBudget
	}
	u.ReservedBytes += charge
	u.Entries += int64(len(p.Entries))
	raw, err = json.Marshal(u)
	if err != nil {
		return err
	}
	if err := tx.Bucket(metaBucket).Put(budgetKey, raw); err != nil {
		return err
	}
	return b.Put([]byte(p.ID), digest[:])
}

func publicationCharge(tx *bolt.Tx, p Proposal) (int64, error) {
	charge := budgetOperationOverhead
	for _, e := range p.Entries {
		// Size is validated before entry. Two target copies can coexist while
		// publishing; the old visible inode can remain as displaced history.
		charge += budgetEntryOverhead + 2*e.Next.Size
		if e.Expected != "" {
			base, err := readVersion(tx, e.Expected)
			if err != nil {
				return 0, err
			}
			if base.Path != e.Path {
				return 0, ErrIdentity
			}
			charge += base.Size
		}
	}
	return charge, nil
}
