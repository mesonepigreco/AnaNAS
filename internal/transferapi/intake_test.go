package transferapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

func TestTLSBudgetRefusesBeforeDeltaReadAndAllowsSmallerOperation(t *testing.T) {
	f := setup(t, true, nil)
	if err := f.c.ConfigurePublicationBudget(journal.PublicationBudget{MaxBytes: (192 << 10) + 2, MaxEntries: 2}); err != nil {
		t.Fatal(err)
	}
	data := []byte("too large")
	wire := encoded(t, nil, data)
	request, _ := f.request(t, "too-large", "file", "", data, wire)
	var body bytes.Buffer
	if err := WriteFrame(&body, request, journal.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	// Intentionally omit the declared delta. Budget rejection must precede the
	// decoder's EOF error and any candidate creation.
	if status, result, _ := f.call(t, "/v1/apply", body.Bytes()); status != http.StatusInsufficientStorage {
		t.Fatal("budget did not reject before reading delta", status, string(result))
	}
	if entries, err := os.ReadDir(f.state); err != nil || len(entries) != 1 || entries[0].Name() != "journal.db" {
		t.Fatal("budget refusal created candidate", entries, err)
	}
	if entries, err := os.ReadDir(f.root); err != nil || len(entries) != 0 {
		t.Fatal("budget refusal published", entries, err)
	}
	data = []byte("x")
	wire = encoded(t, nil, data)
	request, _ = f.request(t, "small", "file", "", data, wire)
	body.Reset()
	if err := WriteFrame(&body, request, journal.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	body.Write(wire)
	if status, result, _ := f.call(t, "/v1/apply", body.Bytes()); status != 200 {
		t.Fatal("refusal blocked subsequent fitting upload", status, string(result))
	}
	if actual, err := os.ReadFile(filepath.Join(f.root, "file")); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("smaller upload differs", err)
	}
	u, err := f.c.PublicationBudgetUsage()
	if err != nil || u == nil || u.ReservedBytes != (192<<10)+2 || u.Entries != 1 {
		t.Fatal("incorrect TLS budget accounting", u, err)
	}
}

func TestTLSPartialUploadRetryReusesOwnedCandidates(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "retry"
		if corrupt {
			name = "corrupt-retained"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t, true, nil)
			data := []byte("first candidate")
			wire := encoded(t, nil, data)
			request, _ := f.request(t, "partial-batch", "first", "", data, wire)
			second, _ := f.request(t, "second", "second", "", data, wire)
			request.Proposal.Entries = append(request.Proposal.Entries, second.Proposal.Entries[0])
			request.DeltaBytes = append(request.DeltaBytes, int64(len(wire)))
			var body bytes.Buffer
			if err := WriteFrame(&body, request, journal.MaxRecordBytes); err != nil {
				t.Fatal(err)
			}
			body.Write(wire)
			body.Write(wire[:len(wire)/2])
			if status, result, _ := f.call(t, "/v1/apply", body.Bytes()); status == 200 {
				t.Fatal("truncated upload committed", string(result))
			}
			firstID := request.Proposal.Entries[0].Next.ID
			firstPath := filepath.Join(f.state, firstID+".ready")
			before, err := os.Stat(firstPath)
			if err != nil {
				t.Fatal("verified first candidate not retained", err)
			}
			if r, err := f.c.Operation(request.Proposal.ID); err != nil || r != nil {
				t.Fatal("incomplete intake marked prepared", r, err)
			}
			other, _ := f.request(t, "other", "other", "", data, wire)
			if _, err := f.c.Commit(context.Background(), f.c.Epoch(), other.Proposal, f.publisher); !errors.Is(err, journal.ErrPending) {
				t.Fatal("another operation bypassed intake", err)
			}
			if corrupt {
				if err := os.Chmod(firstPath, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(firstPath, bytes.Repeat([]byte("x"), len(data)), 0400); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(firstPath, 0400); err != nil {
					t.Fatal(err)
				}
			}
			body.Reset()
			if err := WriteFrame(&body, request, journal.MaxRecordBytes); err != nil {
				t.Fatal(err)
			}
			body.Write(wire)
			body.Write(wire)
			status, result, _ := f.call(t, "/v1/apply", body.Bytes())
			if corrupt {
				if status == 200 {
					t.Fatal("corrupt owned candidate published")
				}
				if _, err := os.Stat(filepath.Join(f.root, "first")); !os.IsNotExist(err) {
					t.Fatal("corrupt retry modified visible root", err)
				}
				return
			}
			if status != 200 {
				t.Fatal(status, string(result))
			}
			var response ApplyResponse
			if err := json.Unmarshal(result, &response); err != nil {
				t.Fatal(err)
			}
			if !response.Record.Committed || response.Record.Sequence != 1 || response.Transfers[0].LiteralBytes != 0 || response.Transfers[1].LiteralBytes != int64(len(data)) {
				t.Fatal(response)
			}
			after, err := os.Stat(firstPath)
			if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
				t.Fatal("retry rewrote retained candidate", err)
			}
			for _, path := range []string{"first", "second"} {
				actual, err := os.ReadFile(filepath.Join(f.root, path))
				if err != nil || !bytes.Equal(actual, data) {
					t.Fatal("retry published different content", path, err)
				}
			}
		})
	}
}

func TestTLSWrongDigestDoesNotSealCandidate(t *testing.T) {
	f := setup(t, true, nil)
	wire := encoded(t, nil, []byte("wrong"))
	request, _ := f.request(t, "digest", "file", "", []byte("right"), wire)
	var body bytes.Buffer
	if err := WriteFrame(&body, request, journal.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	body.Write(wire)
	if status, _, _ := f.call(t, "/v1/apply", body.Bytes()); status == 200 {
		t.Fatal("wrong digest accepted")
	}
	path := filepath.Join(f.state, request.Proposal.Entries[0].Next.ID+".ready")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("wrong digest sealed candidate", err)
	}
	wire = encoded(t, nil, []byte("right"))
	body.Reset()
	if err := WriteFrame(&body, request, journal.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	body.Write(wire)
	if status, result, _ := f.call(t, "/v1/apply", body.Bytes()); status != 200 {
		t.Fatal("corrected exact intake could not resume", status, string(result))
	}
}

func TestServerRejectsUncoordinatedStagingDirectory(t *testing.T) {
	f := setup(t, true, nil)
	dir := filepath.Join(t.TempDir(), "other")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := stage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := New(f.c, store, f.publisher, f.api.opts); err == nil {
		t.Fatal("uncoordinated staging directory accepted")
	}
}
