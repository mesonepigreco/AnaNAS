package transferapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
)

func TestTLSAcknowledgementBindsClientPolicyAndCommittedPrefix(t *testing.T) {
	f := setup(t, true, nil)
	for i := 0; i < 3; i++ {
		p := journal.Proposal{ID: identifier(fmt.Sprint("ack-op", i)), Client: identifier("author"), Entries: []journal.Entry{{Path: fmt.Sprint("file", i), Next: journal.Version{ID: identifier(fmt.Sprint("ack-v", i)), Digest: hash.SumBytes(nil)}}}}
		if _, err := f.c.Commit(context.Background(), f.c.Epoch(), p, metadataPublisher{}); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestClient(t, clientOptions(f))
	page, err := c.ChangesPage(context.Background(), "", "", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := f.c.Cursor(f.clientID); err != nil || n != 0 {
		t.Fatal("fetch implicitly acknowledged", n, err)
	}
	if err := c.AcknowledgeChanges(context.Background(), page.Namespace, page.Policy, 0, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.AcknowledgeChanges(context.Background(), page.Namespace, page.Policy, 0, 2); err != nil {
		t.Fatal("lost reply retry failed", err)
	}
	if n, err := f.c.Cursor(f.clientID); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if n, err := f.c.Cursor(identifier("other")); err != nil || n != 0 {
		t.Fatal("other client advanced", n, err)
	}
	o := clientOptions(f)
	o.Exclusions = []string{"file2"}
	changed := newTestClient(t, o)
	policy := changefeed.Policy(page.Namespace, nil, o.Exclusions)
	if err := changed.AcknowledgeChanges(context.Background(), page.Namespace, policy, 2, 3); err == nil {
		t.Fatal("changed exclusions reused cursor")
	}
	if err := c.AcknowledgeChanges(context.Background(), page.Namespace, page.Policy, 2, 4); err == nil {
		t.Fatal("future batch acknowledged")
	}
	var body bytes.Buffer
	forged := map[string]any{"namespace": page.Namespace, "policy": page.Policy, "expected": 2, "through": 3, "client": identifier("other"), "exclusions": []string{}}
	if err := WriteFrame(&body, forged, changefeed.MaxRequestBytes); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := f.call(t, "/v1/ack", body.Bytes()); status != 400 {
		t.Fatal("body client identity accepted", status)
	}
	if err := c.AcknowledgeChanges(context.Background(), page.Namespace, page.Policy, 2, 3); err != nil {
		t.Fatal(err)
	}
}

func TestAcknowledgementDisabledAndInvalidRequestsDoNotAdvance(t *testing.T) {
	f := setup(t, false, nil)
	p := journal.Proposal{ID: identifier("read-only-ack-op"), Client: identifier("author"), Entries: []journal.Entry{{Path: "file", Next: journal.Version{ID: identifier("read-only-ack-v"), Digest: hash.SumBytes(nil)}}}}
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), p, metadataPublisher{}); err != nil {
		t.Fatal(err)
	}
	o := clientOptions(f)
	c := newTestClient(t, o)
	policy := changefeed.Policy(f.c.Namespace(), nil, nil)
	if err := c.AcknowledgeChanges(context.Background(), f.c.Namespace(), policy, 0, 1); err == nil {
		t.Fatal("read-only server accepted acknowledgement")
	} else {
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.Status != http.StatusForbidden {
			t.Fatal("read-only guard did not reject valid committed ACK", err)
		}
	}
	o.Writes = false
	readonly := newTestClient(t, o)
	if err := readonly.AcknowledgeChanges(context.Background(), f.c.Namespace(), policy, 0, 1); err == nil {
		t.Fatal("read-only client sent acknowledgement")
	}
	if readonly.Traffic() != (Traffic{}) {
		t.Fatal("read-only client contacted NAS")
	}
	if n, err := f.c.Cursor(f.clientID); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
