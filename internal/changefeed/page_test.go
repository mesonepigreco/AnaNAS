package changefeed

import (
	"encoding/json"
	"strings"
	"testing"

	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
)

func id(s string) string { return hash.SumBytes([]byte(s)).Hex() }
func record(n uint64, paths ...string) journal.Record {
	r := journal.Record{Proposal: journal.Proposal{ID: id(string(rune(n))), Client: id("client")}, Sequence: n, Epoch: 1, Committed: true}
	for _, path := range paths {
		r.Entries = append(r.Entries, journal.Entry{Path: path, Next: journal.Version{ID: id(path), Digest: hash.SumBytes(nil)}})
	}
	return r
}

func TestFilterPreservesOrderWithoutExcludedIdentities(t *testing.T) {
	r := record(1, "visible", "client-secret/hidden", "server-secret/hidden")
	second := record(2, "client-secret")
	second.Entries[0].Next = journal.Version{ID: id("deleted directory"), Tombstone: true}
	request := Request{Limit: 2, Exclusions: []string{"client-secret/"}}
	page, err := Filter(id("namespace"), []string{"server-secret/"}, request, []journal.Record{r, second})
	if err != nil {
		t.Fatal(err)
	}
	if page.Through != 2 || len(page.Batches) != 2 || len(page.Batches[0].Entries) != 1 || page.Batches[0].Excluded != 2 || len(page.Batches[1].Entries) != 0 || page.Batches[1].Excluded != 1 {
		t.Fatal(page)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), id("client-secret/hidden")) {
		t.Fatal("excluded path/version leaked")
	}
	if len(r.Entries) != 3 {
		t.Fatal("filter modified journal input")
	}
	request.Namespace, request.Policy, request.After = page.Namespace, page.Policy, 2
	if _, err := Filter(page.Namespace, []string{"server-secret/"}, request, nil); err != nil {
		t.Fatal(err)
	}
	request.Exclusions = nil
	if _, err := Filter(page.Namespace, []string{"server-secret/"}, request, nil); err == nil {
		t.Fatal("silently continued after exclusion change")
	}
}

func TestPageRejectsGapsAndInvalidBatches(t *testing.T) {
	for _, mutate := range []func(*Page){
		func(p *Page) { p.Through++ },
		func(p *Page) { p.Batches[0].Sequence++ },
		func(p *Page) { p.Batches[0].Committed = false },
		func(p *Page) { p.Batches[0].Excluded = -1 },
		func(p *Page) { p.Batches[0].Excluded = journal.MaxEntries },
		func(p *Page) { p.Batches[0].Entries[0].Path = "../escape" },
		func(p *Page) { p.Batches[0].Entries = nil },
	} {
		page, err := Filter(id("namespace"), nil, Request{Limit: 1}, []journal.Record{record(1, "file")})
		if err != nil {
			t.Fatal(err)
		}
		mutate(&page)
		if err := page.Validate(0, 1); err == nil {
			t.Fatal("accepted invalid page", page)
		}
	}
	if err := (Request{After: 1, Limit: 1}).Validate(); err == nil {
		t.Fatal("unbound continuation accepted")
	}
	if _, err := Patterns([]string{strings.Repeat("x", 4097)}); err == nil {
		t.Fatal("oversized pattern accepted")
	}
	if _, err := Patterns(make([]string, 129)); err == nil {
		t.Fatal("unbounded pattern count")
	}
}
