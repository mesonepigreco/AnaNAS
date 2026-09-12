package transferapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/content"
	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/publish"
	"nas-sync/internal/replica"
	"nas-sync/internal/stage"
)

func TestTLSUploadCompletionAllowsLaterPull(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "pull-update"
		if edited {
			name = "preserve-newer-local-edit"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			root, state := filepath.Join(dir, "local"), filepath.Join(dir, "state")
			for _, path := range []string{root, state} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			db, err := index.Open(filepath.Join(state, "index.db"), root)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			clientID, err := db.ClientID()
			if err != nil {
				t.Fatal(err)
			}
			f := setupWithClientID(t, true, nil, clientID)
			client := newTestClient(t, clientOptions(f))
			c, err := journal.Open(state, identifier("local journal"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			store, err := stage.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			publisher, err := publish.Open(root, state, c.Version, nil, 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			defer publisher.Close()
			data := make([]byte, 256*1024)
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "file")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, false); err != nil {
				t.Fatal(err)
			}
			wire := encoded(t, nil, data)
			request, _ := f.request(t, "upload", "file", "", data, wire)
			v := request.Proposal.Entries[0].Next
			ctx := context.Background()
			if err := store.ReceiveVerified(ctx, v.ID, v.Size, v.Digest, func(w io.Writer) error { _, err := w.Write(data); return err }); err != nil {
				t.Fatal(err)
			}
			if err := db.PrepareUpload(index.Upload{Namespace: f.c.Namespace(), Proposal: request.Proposal, Generations: []uint64{1}, DeltaBytes: request.DeltaBytes, DeltaDigests: []hash.Digest{hash.SumBytes(wire)}}); err != nil {
				t.Fatal(err)
			}
			result, err := client.Apply(ctx, request, []io.Reader{bytes.NewReader(wire)})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.RecordUploadCommit(f.c.Namespace(), result.Record); err != nil {
				t.Fatal(err)
			}
			traffic := client.Traffic()
			if err := replica.FinishUpload(ctx, db, c, store, f.c.Namespace(), pushOptions()); err != nil {
				t.Fatal(err)
			}
			if client.Traffic() != traffic {
				t.Fatal("local completion generated traffic")
			}
			if edited {
				if err := os.WriteFile(path, []byte("new local edit"), 0600); err != nil {
					t.Fatal(err)
				}
				st, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Put([]index.Record{{Path: "file", Fingerprint: content.Fingerprint(st)}}, true); err != nil {
					t.Fatal(err)
				}
			}
			target := append([]byte{17}, data...)
			wire = encoded(t, data, target)
			next, _ := f.request(t, "remote-update", "file", v.ID, target, wire)
			if _, err := client.Apply(ctx, next, []io.Reader{bytes.NewReader(wire)}); err != nil {
				t.Fatal(err)
			}
			if edited {
				source, err := content.OpenRoot(root, nil)
				if err != nil {
					t.Fatal(err)
				}
				compared, compareErr := replica.Compare(ctx, db, source, c, client, "file", replica.ComparisonOptions{Namespace: f.c.Namespace(), MaxFileBytes: 1 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }})
				source.Close()
				if compareErr != nil || compared.Cleared || compared.Decision.Action != diff.Conflict {
					t.Fatal("independent edits did not compare as a conflict", compared, compareErr)
				}
			}
			puller, err := replica.NewPuller(c, store, publisher, client, replica.PullOptions{Namespace: f.c.Namespace(), Writes: true, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			pull, err := puller.Pull(ctx, next.Proposal)
			if edited {
				if !errors.Is(err, publish.ErrExternal) {
					t.Fatal("newer local edit was not preserved", err)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != "new local edit" {
					t.Fatal("local edit replaced", err)
				}
				return
			}
			if err != nil || !pull.Record.Committed || pull.Transfers[0].LiteralBytes != 1 || pull.Transfers[0].ReusedBytes != int64(len(data)) {
				t.Fatal("upload base could not drive later pull", pull, err)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, target) {
				t.Fatal("pulled content differs", err)
			}
		})
	}
}
