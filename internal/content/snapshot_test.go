package content

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/hash"
	"nas-sync/internal/stage"
)

func snapshotStore(t *testing.T) (*stage.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := stage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestSnapshotStableBytesAndBlockManifest(t *testing.T) {
	for _, size := range []int{0, 65536 + 7} {
		data := bytes.Repeat([]byte{31}, size)
		r, root, fp := localFixture(t, data)
		s, _ := snapshotStore(t)
		snapshot, err := r.Snapshot(context.Background(), "file", fp, s, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Version.Digest != hash.SumBytes(data) || snapshot.Version.ID != snapshot.Manifest.Content.ID || snapshot.Fingerprint != fp {
			t.Fatal(snapshot)
		}
		if err := snapshot.Manifest.Validate(); err != nil {
			t.Fatal(err)
		}
		for i, block := range snapshot.Manifest.Content.Blocks {
			start := i * 65536
			if block.Digest != hash.SumBytes(data[start:min(start+65536, len(data))]) {
				t.Fatal("manifest block differs")
			}
		}
		if err := os.WriteFile(filepath.Join(root, "file"), []byte("later edit"), 0600); err != nil {
			t.Fatal(err)
		}
		f, err := s.OpenReady(snapshot.Version.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(f)
		f.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("source edit changed immutable snapshot", err)
		}
	}
}

func TestSnapshotRejectsStaleSourceAndExcludedPath(t *testing.T) {
	r, root, fp := localFixture(t, []byte("old"))
	s, state := snapshotStore(t)
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Snapshot(context.Background(), "file", fp, s, 1<<30); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	excluded, err := OpenRoot(root, []string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	defer excluded.Close()
	if _, err := excluded.Snapshot(context.Background(), "file", fp, s, 1<<30); err == nil {
		t.Fatal("excluded path captured")
	}
	files, err := os.ReadDir(state)
	if err != nil || len(files) != 0 {
		t.Fatal("failed snapshot created state", files, err)
	}
}
