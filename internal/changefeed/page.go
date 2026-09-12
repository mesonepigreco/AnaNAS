// Package changefeed defines bounded, exclusion-filtered journal pages. It has
// no filesystem or network operations and never grants content publication.
package changefeed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"nas-sync/internal/exclude"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

const MaxRequestBytes = 16 * 1024
const MaxPageBytes = journal.MaxPage*journal.MaxRecordBytes + 4096

type Request struct {
	Namespace  string   `json:"namespace"`
	Policy     string   `json:"policy"`
	After      uint64   `json:"after"`
	Limit      int      `json:"limit"`
	Exclusions []string `json:"exclusions"`
}

// Excluded counts omitted entries without exposing their paths or versions.
// Even an entirely excluded batch retains its sequence and operation identity.
type Batch struct {
	journal.Record
	Excluded int `json:"excluded"`
}

type Page struct {
	Namespace string  `json:"namespace"`
	Policy    string  `json:"policy"`
	After     uint64  `json:"after"`
	Through   uint64  `json:"through"`
	Batches   []Batch `json:"batches"`
}

func Patterns(patterns []string) (*exclude.Matcher, error) {
	if len(patterns) > 128 {
		return nil, fmt.Errorf("at most 128 feed exclusions allowed")
	}
	total := 0
	for _, p := range patterns {
		total += len(p)
		if len(p) > 4096 || total > 8192 {
			return nil, fmt.Errorf("feed exclusions exceed byte limit")
		}
	}
	return exclude.Compile(patterns)
}

func (r Request) Validate() error {
	if r.Limit < 1 || r.Limit > journal.MaxPage {
		return fmt.Errorf("invalid change page limit")
	}
	if (r.Namespace == "") != (r.Policy == "") || (r.After > 0 && r.Namespace == "") {
		return fmt.Errorf("continued replay requires namespace and policy")
	}
	if r.Namespace != "" && (!manifest.ValidID(r.Namespace) || !manifest.ValidID(r.Policy)) {
		return fmt.Errorf("invalid feed identity")
	}
	_, err := Patterns(r.Exclusions)
	return err
}

// Policy binds filtering semantics and both configurations to this namespace.
// Changing either exclusion list requires explicit replay/reconciliation, never
// silently continuing a cursor over newly included paths.
func Policy(namespace string, server, client []string) string {
	data, _ := json.Marshal(struct {
		Format, Namespace string
		Server, Client    []string
	}{"ananas-feed-v1", namespace, append([]string{}, server...), append([]string{}, client...)})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func Filter(namespace string, server []string, request Request, records []journal.Record) (Page, error) {
	var page Page
	if err := request.Validate(); err != nil {
		return page, err
	}
	a, err := Patterns(server)
	if err != nil {
		return page, err
	}
	b, err := Patterns(request.Exclusions)
	if err != nil {
		return page, err
	}
	policy := Policy(namespace, server, request.Exclusions)
	if !manifest.ValidID(namespace) || (request.Namespace != "" && (request.Namespace != namespace || request.Policy != policy)) {
		return page, fmt.Errorf("feed namespace or exclusion policy changed")
	}
	if len(records) > request.Limit {
		return Page{}, fmt.Errorf("too many feed records")
	}
	page = Page{Namespace: namespace, Policy: policy, After: request.After, Through: request.After, Batches: make([]Batch, 0, len(records))}
	for _, record := range records {
		if err := journal.ValidateProposal(record.Proposal); err != nil {
			return Page{}, err
		}
		batch := Batch{Record: record}
		batch.Entries = make([]journal.Entry, 0, len(record.Entries))
		for _, e := range record.Entries {
			// A tombstone may represent a directory. Conservatively apply directory
			// exclusions without fetching any former content or listing a path.
			isDir := e.Next.Directory || e.Next.Tombstone
			if a.Match(e.Path, isDir) || b.Match(e.Path, isDir) {
				batch.Excluded++
				continue
			}
			batch.Entries = append(batch.Entries, e)
		}
		page.Batches = append(page.Batches, batch)
		page.Through = record.Sequence
	}
	if err := page.Validate(request.After, request.Limit); err != nil {
		return Page{}, err
	}
	return page, nil
}

func (p Page) Validate(after uint64, limit int) error {
	if !manifest.ValidID(p.Namespace) || !manifest.ValidID(p.Policy) || p.After != after || limit < 1 || limit > journal.MaxPage || len(p.Batches) > limit {
		return fmt.Errorf("invalid feed page identity or bounds")
	}
	n := after
	for _, b := range p.Batches {
		if n == ^uint64(0) || b.Sequence != n+1 || !b.Committed || b.Epoch == 0 || !manifest.ValidID(b.ID) || !manifest.ValidID(b.Client) {
			return fmt.Errorf("invalid or noncontiguous feed batch")
		}
		n++
		if b.Excluded < 0 || b.Excluded > journal.MaxEntries || len(b.Entries)+b.Excluded < 1 || len(b.Entries)+b.Excluded > journal.MaxEntries {
			return fmt.Errorf("invalid feed entry count")
		}
		if len(b.Entries) > 0 {
			if err := journal.ValidateProposal(b.Proposal); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(b)
		if err != nil || len(raw) > journal.MaxRecordBytes {
			return fmt.Errorf("feed batch exceeds metadata limit")
		}
	}
	if p.Through != n {
		return fmt.Errorf("feed cursor differs from batches")
	}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > MaxPageBytes {
		return fmt.Errorf("feed page exceeds metadata limit")
	}
	return nil
}
