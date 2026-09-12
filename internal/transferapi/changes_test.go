package transferapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

type metadataPublisher struct{}

func (metadataPublisher) Publish(context.Context, journal.Record) error { return nil }

func TestTLSChangeFeedAndDurableInbox(t *testing.T) {
	f := setup(t, false, []string{"server-secret/"})
	// Metadata fixtures exercise feed filtering only, not content publication.
	for i, paths := range [][]string{{"visible", "client-secret/hidden", "server-secret/hidden"}, {"client-secret/another"}} {
		p := journal.Proposal{ID: identifier(string(rune(i + 1))), Client: identifier("another-client")}
		for _, path := range paths {
			p.Entries = append(p.Entries, journal.Entry{Path: path, Next: journal.Version{ID: identifier(path), Digest: hash.SumBytes(nil)}})
		}
		if _, err := f.c.Commit(context.Background(), f.c.Epoch(), p, metadataPublisher{}); err != nil {
			t.Fatal(err)
		}
	}
	o := clientOptions(f)
	o.Exclusions = []string{"client-secret/"}
	c := newTestClient(t, o)
	page, err := c.ChangesPage(context.Background(), "", "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Through != 2 || page.Batches[0].Excluded != 2 || page.Batches[1].Excluded != 1 {
		t.Fatal(page)
	}
	data, _ := json.Marshal(page)
	if strings.Contains(string(data), "secret") {
		t.Fatal("excluded metadata leaked")
	}
	d, err := index.Open(filepath.Join(t.TempDir(), "index.db"), "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	state, err := d.RemoteState()
	if err != nil || state.Received != 2 || state.Completed != 0 {
		t.Fatal(state, err)
	}
	if cursor, err := f.c.Cursor(f.clientID); err != nil || cursor != 0 {
		t.Fatal("fetch implicitly acknowledged", cursor, err)
	}
	if _, err := c.ChangesPage(context.Background(), page.Namespace, identifier("changed policy"), 2, 1); err == nil {
		t.Fatal("changed policy accepted")
	}
	if _, err := c.ChangesPage(context.Background(), page.Namespace, page.Policy, 3, 1); err == nil {
		t.Fatal("future cursor accepted")
	}
	empty, err := c.ChangesPage(context.Background(), page.Namespace, page.Policy, 2, 1)
	if err != nil || len(empty.Batches) != 0 || empty.Through != 2 {
		t.Fatal(empty, err)
	}
}

func TestClientRejectsInvalidFeed(t *testing.T) {
	for _, mode := range []string{"gap", "excluded path", "changed namespace"} {
		t.Run(mode, func(t *testing.T) {
			namespace, policy := identifier("namespace"), identifier("policy")
			batch := changefeed.Batch{Record: journal.Record{Proposal: journal.Proposal{ID: identifier("op"), Client: identifier("client"), Entries: []journal.Entry{{Path: "secret/file", Next: journal.Version{ID: identifier("version"), Digest: hash.SumBytes(nil)}}}}, Sequence: 1, Epoch: 1, Committed: true}}
			page := changefeed.Page{Namespace: namespace, Policy: policy, Through: 1, Batches: []changefeed.Batch{batch}}
			if mode == "gap" {
				page.Batches[0].Sequence = 2
			}
			if mode == "changed namespace" {
				page.Namespace = identifier("other")
			}
			c := clientWithPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reply(w, 200, page) }))
			if mode == "excluded path" {
				// Configure a fresh client so the compiled matcher and request agree.
				o := c.opts
				o.Exclusions = []string{"secret/"}
				c = newTestClient(t, o)
			}
			if _, err := c.ChangesPage(context.Background(), namespace, policy, 0, 1); err == nil {
				t.Fatal("accepted invalid feed")
			}
		})
	}
}

func TestFeedRequestBounds(t *testing.T) {
	f := setup(t, false, nil)
	c := newTestClient(t, clientOptions(f))
	if _, err := c.ChangesPage(context.Background(), "", "", 1, 1); err == nil {
		t.Fatal("unbound replay accepted")
	}
	if _, err := c.ChangesPage(context.Background(), "", "", 0, journal.MaxPage+1); err == nil {
		t.Fatal("oversized page accepted")
	}
	if c.Traffic() != (Traffic{}) {
		t.Fatal("invalid request caused traffic")
	}
	_, err := c.ChangesPage(context.Background(), identifier("wrong namespace"), identifier("policy"), 0, 1)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Status != 409 {
		t.Fatal(err)
	}
}
