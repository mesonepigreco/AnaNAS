package content

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
	"nas-sync/internal/index"
	"nas-sync/internal/manifest"
)

func TestDirectoryManifestCannotOpenEmptyFileCandidate(t *testing.T) {
	r, _, fp := localFixture(t, nil)
	m := &manifest.Manifest{BlockSize: 65536, Content: diff.Manifest{ID: hash.SumBytes([]byte("directory")).Hex(), Directory: true}}
	if candidate, err := r.OpenCandidate("file", fp, m); err == nil {
		candidate.Close()
		t.Fatal("directory manifest accepted as empty-file content")
	}
}

func localFixture(t *testing.T, data []byte) (*Root, string, index.Fingerprint) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoot(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	st, err := os.Stat(filepath.Join(dir, "file"))
	if err != nil {
		t.Fatal(err)
	}
	return r, dir, Fingerprint(st)
}

func TestStableManifestAndVerifiedRanges(t *testing.T) {
	data := append(bytes.Repeat([]byte("a"), 1024), []byte("tail")...)
	r, _, fp := localFixture(t, data)
	m, err := r.Hash(context.Background(), "file", fp, 1024, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if m.Content.Size != int64(len(data)) || len(m.Content.Blocks) != 2 {
		t.Fatalf("wrong manifest: %+v", m)
	}
	c, err := r.OpenCandidate("file", fp, m)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Callers cannot change a candidate's immutable digest list after opening.
	m.Content.Blocks[1].Digest = hash.Digest{}
	b, err := c.ReadBlock(context.Background(), 1, make([]byte, 1024))
	if err != nil || !bytes.Equal(b, []byte("tail")) {
		t.Fatalf("wrong range: %q %v", b, err)
	}
	if _, err := c.ReadBlock(context.Background(), 1, make([]byte, 1)); err == nil {
		t.Fatal("short buffer accepted")
	}
	if err := c.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestChangedBytesNeverLeaveCandidateUnderOldDigest(t *testing.T) {
	r, dir, fp := localFixture(t, bytes.Repeat([]byte("a"), 2048))
	m, err := r.Hash(context.Background(), "file", fp, 1024, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.OpenCandidate("file", fp, m)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := os.WriteFile(filepath.Join(dir, "file"), bytes.Repeat([]byte("b"), 2048), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := c.ReadBlock(context.Background(), 0, make([]byte, 1024)); !errors.Is(err, ErrChanged) || b != nil {
		t.Fatalf("changed bytes exposed: %d %v", len(b), err)
	}
	if err := c.Verify(); !errors.Is(err, ErrChanged) {
		t.Fatal("changed fingerprint accepted", err)
	}
	if _, err := r.Hash(context.Background(), "file", fp, 1024, 1<<30); !errors.Is(err, ErrChanged) {
		t.Fatal("stale index fingerprint accepted", err)
	}
}

func TestRejectSymlinksSpecialFilesAndExclusions(t *testing.T) {
	r, dir, fp := localFixture(t, []byte("content"))
	if err := os.Mkdir(filepath.Join(dir, "skip"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skip", "file"), []byte("hidden"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("skip", filepath.Join(dir, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"link", "linked-dir/file", "fifo", "skip", "../escape", "/absolute"} {
		if _, err := r.Hash(context.Background(), p, fp, 1024, 1<<30); err == nil {
			t.Fatalf("unsafe path accepted: %s", p)
		}
	}
	excluded, err := OpenRoot(dir, []string{"skip/"})
	if err != nil {
		t.Fatal(err)
	}
	defer excluded.Close()
	if _, err := excluded.open("skip/file"); err == nil {
		t.Fatal("excluded child opened")
	}
	if _, err := excluded.open(".nas-sync/private"); err == nil {
		t.Fatal("internal metadata opened")
	}
}

func TestRootReplacementInvalidatesCandidate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoot(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	st, err := os.Stat(filepath.Join(dir, "file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.Verify("file", Fingerprint(st)); !errors.Is(err, ErrChanged) {
		t.Fatal("replaced root accepted", err)
	}
}

func TestHashRejectsWriteDuringPacedRead(t *testing.T) {
	r, dir, fp := localFixture(t, bytes.Repeat([]byte("a"), 2048))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.Hash(ctx, "file", fp, 1024, 1024); done <- err }()
	// The two-block candidate takes at least one second at this test rate.
	// Change it during that paced window, after the initial fingerprint check.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "file"), bytes.Repeat([]byte("b"), 2048), 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrChanged) {
		t.Fatal("unstable candidate accepted", err)
	}
}

func TestPacingAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &pacedReader{ctx: ctx, r: bytes.NewReader(make([]byte, 2048)), rate: 1024}
	if n, err := r.Read(make([]byte, 1024)); n != 1024 || err != nil {
		t.Fatal(n, err)
	}
	if remaining := time.Until(r.next); remaining < 900*time.Millisecond {
		t.Fatal("read budget not enforced", remaining)
	}
	cancel()
	start := time.Now()
	if _, err := r.Read(make([]byte, 1024)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("cancellation waited for read budget")
	}
}

func TestOversizedCandidateRejectedBeforeOpen(t *testing.T) {
	r, _, fp := localFixture(t, nil)
	fp.Size = int64(manifest.MaxBlocks)*1024 + 1
	if _, err := r.Hash(context.Background(), "does-not-exist", fp, 1024, 1024); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversized candidate opened before bounds validation", err)
	}
}
