package stage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
)

var testID = strings.Repeat("a", 64)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func testWire(t *testing.T) ([]byte, []byte) {
	t.Helper()
	target := bytes.Repeat([]byte("verified content\n"), 160)
	base, err := delta.Build(context.Background(), bytes.NewReader(nil), 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if _, err := delta.Encode(context.Background(), bytes.NewReader(target), int64(len(target)), base, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Bytes(), target
}

func TestReceiveReadyAndDuplicate(t *testing.T) {
	s, path := newStore(t)
	wire, target := testWire(t)
	stats, err := s.Receive(context.Background(), testID, bytes.NewReader(wire), nil, 0, hash.SumBytes(nil))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Digest != hash.SumBytes(target) {
		t.Fatal("wrong digest")
	}
	if _, err := os.Lstat(filepath.Join(path, testID+".partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial survived success", err)
	}
	f, err := s.OpenReady(testID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil || !bytes.Equal(got, target) {
		t.Fatal("ready bytes differ", err)
	}
	if _, err := s.Receive(context.Background(), testID, bytes.NewReader(wire), nil, 0, hash.SumBytes(nil)); !errors.Is(err, os.ErrExist) {
		t.Fatal("duplicate replaced ready object", err)
	}
	// Recovery can discard an absent partial without changing its ready peer.
	if err := s.DiscardPartial(testID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, testID+".ready")); err != nil {
		t.Fatal(err)
	}
}

func TestFailedTransferNeverReady(t *testing.T) {
	wire, _ := testWire(t)
	for _, end := range []int{0, 60, 100, len(wire) - 1} {
		t.Run(fmt.Sprint(end), func(t *testing.T) {
			s, path := newStore(t)
			if _, err := s.Receive(context.Background(), testID, bytes.NewReader(wire[:end]), nil, 0, hash.SumBytes(nil)); err == nil {
				t.Fatal("accepted incomplete stream")
			}
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed transfer left candidate", entries, err)
			}
		})
	}
	s, path := newStore(t)
	bad := bytes.Clone(wire)
	bad[len(bad)-1] ^= 1
	if _, err := s.Receive(context.Background(), testID, bytes.NewReader(bad), nil, 0, hash.SumBytes(nil)); err == nil {
		t.Fatal("accepted corrupt digest")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Receive(ctx, testID, bytes.NewReader(wire), nil, 0, hash.SumBytes(nil)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(path)
	if len(entries) != 0 {
		t.Fatal("canceled/corrupt transfer left objects")
	}
}

func TestRejectUnsafePathsAndObjects(t *testing.T) {
	s, path := newStore(t)
	wire, _ := testWire(t)
	for _, id := range []string{"", "../file", strings.Repeat("A", 64), strings.Repeat("a", 65)} {
		if _, err := s.Receive(context.Background(), id, bytes.NewReader(wire), nil, 0, hash.SumBytes(nil)); err == nil {
			t.Fatal("accepted invalid ID")
		}
		if _, err := s.OpenReady(id); err == nil {
			t.Fatal("opened invalid ID")
		}
		if err := s.DiscardPartial(id); err == nil {
			t.Fatal("discarded invalid ID")
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(path, testID+".partial")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Receive(context.Background(), testID, bytes.NewReader(wire), nil, 0, hash.SumBytes(nil)); err == nil {
		t.Fatal("followed partial symlink")
	}
	if err := s.DiscardPartial(testID); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "unchanged" {
		t.Fatal("modified symlink target", err)
	}
	if err := os.Symlink(outside, filepath.Join(path, testID+".ready")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenReady(testID); err == nil {
		t.Fatal("followed ready symlink")
	}
	if _, err := s.Receive(context.Background(), testID, bytes.NewReader(wire), nil, 0, hash.SumBytes(nil)); !errors.Is(err, os.ErrExist) {
		t.Fatal("replaced ready symlink", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Dir(path), link); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(filepath.Join(link, "stage")); err == nil {
		opened.Close()
		t.Fatal("followed ancestor symlink")
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(path); err == nil {
		opened.Close()
		t.Fatal("accepted nonprivate store")
	}
}

func TestRecoverPartialAfterLink(t *testing.T) {
	s, path := newStore(t)
	partial, ready := filepath.Join(path, testID+".partial"), filepath.Join(path, testID+".ready")
	// Model the narrow crash boundary between successful link and unlink.
	if err := os.WriteFile(partial, []byte("verified"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(partial, ready); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenReady(testID); err == nil {
		t.Fatal("accepted unfinished linked pair")
	}
	if err := s.DiscardPartial(testID); err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenReady(testID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

type stopBeforeTrailer struct{ data *bytes.Reader }

func (r *stopBeforeTrailer) Read(p []byte) (int, error) {
	if r.data.Len() > 0 {
		return r.data.Read(p)
	}
	fmt.Fprintln(os.Stdout, "partial-written")
	// The parent kills this process, bypassing Receive's deferred cleanup.
	for {
		time.Sleep(time.Hour)
	}
}

func TestKilledReceiverRecovery(t *testing.T) {
	if path := os.Getenv("ANANAS_STAGE_CRASH_TEST"); path != "" {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := testWire(t)
		_, err = s.Receive(context.Background(), testID, &stopBeforeTrailer{bytes.NewReader(wire[:len(wire)-33])}, nil, 0, hash.SumBytes(nil))
		t.Fatalf("receiver unexpectedly returned: %v", err)
	}
	s, path := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKilledReceiverRecovery$")
	cmd.Env = append(os.Environ(), "ANANAS_STAGE_CRASH_TEST="+path)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(pipe).ReadString('\n')
	cmd.Process.Kill()
	cmd.Wait()
	if err != nil || line != "partial-written\n" {
		t.Fatalf("child did not reach crash boundary: %q %v", line, err)
	}
	if _, err := s.OpenReady(testID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("interrupted receive appears ready", err)
	}
	if _, err := os.Stat(filepath.Join(path, testID+".partial")); err != nil {
		t.Fatal("missing crash artifact", err)
	}
	if err := s.DiscardPartial(testID); err != nil {
		t.Fatal(err)
	}
	wire, _ := testWire(t)
	if _, err := s.Receive(context.Background(), testID, bytes.NewReader(wire), nil, 0, hash.SumBytes(nil)); err != nil {
		t.Fatal("retry after recovery failed", err)
	}
}
