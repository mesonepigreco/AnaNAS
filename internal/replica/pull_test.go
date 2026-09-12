package replica

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/publish"
	"nas-sync/internal/stage"
)

func id(s string) string { return hash.SumBytes([]byte(s)).Hex() }

type source struct {
	content map[string][]byte
	calls   int
	fail    bool
	failOn  int
}

func (s *source) DownloadVersion(ctx context.Context, path string, want journal.Version, base *delta.Signature, old io.ReaderAt, out io.Writer) (delta.Stats, error) {
	s.calls++
	data := s.content[want.ID]
	var wire bytes.Buffer
	_, err := delta.Encode(ctx, bytes.NewReader(data), int64(len(data)), base, &wire)
	if err != nil {
		return delta.Stats{}, err
	}
	stats, err := delta.Apply(ctx, &wire, old, base.Size, base.Digest, out)
	if err == nil && (s.fail || s.calls == s.failOn) {
		err = errors.New("late download failure")
	}
	return stats, err
}

type fixture struct {
	root, state string
	c           *journal.Coordinator
	store       *stage.Store
	publisher   *publish.Publisher
	puller      *Puller
	remote      *source
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{root: filepath.Join(dir, "root"), state: filepath.Join(dir, "state"), remote: &source{content: map[string][]byte{}}}
	for _, path := range []string{f.root, f.state} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.c, err = journal.Open(f.state, id("namespace"))
	if err != nil {
		t.Fatal(err)
	}
	f.store, err = stage.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	f.publisher, err = publish.Open(f.root, f.state, f.c.Version, nil, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	f.puller, err = NewPuller(f.c, f.store, f.publisher, f.remote, PullOptions{Namespace: id("remote namespace"), Writes: true, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, ReadBytesPerSecond: 1 << 30, Gate: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.publisher.Close(); f.store.Close(); f.c.Close() })
	return f
}
func (f *fixture) proposal(name, path, expected string, data []byte) journal.Proposal {
	v := journal.Version{ID: id(name + "version"), Size: int64(len(data)), Digest: hash.SumBytes(data)}
	f.remote.content[v.ID] = data
	return journal.Proposal{ID: id(name), Client: id("remote-client"), Entries: []journal.Entry{{Path: path, Expected: expected, Next: v}}}
}

func TestPullPublishesDirectoryDiffAndExplicitDeletes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	folder := f.proposal("mkdir", "folder", "", nil)
	folder.Entries[0].Next = journal.Version{ID: id("directory"), Directory: true}
	if _, err := f.puller.Pull(ctx, folder); err != nil {
		t.Fatal(err)
	}
	base := make([]byte, 256*1024)
	if _, err := rand.Read(base); err != nil {
		t.Fatal(err)
	}
	first := f.proposal("create", "folder/file", "", base)
	if _, err := f.puller.Pull(ctx, first); err != nil {
		t.Fatal(err)
	}
	target := append([]byte{42}, base...)
	second := f.proposal("edit", "folder/file", first.Entries[0].Next.ID, target)
	result, err := f.puller.Pull(ctx, second)
	if err != nil || !result.Record.Committed || result.Transfers[0].LiteralBytes != 1 || result.Transfers[0].ReusedBytes != int64(len(base)) {
		t.Fatal(result, err)
	}
	actual, err := os.ReadFile(filepath.Join(f.root, "folder/file"))
	if err != nil || !bytes.Equal(actual, target) {
		t.Fatal("published content differs", err)
	}
	if _, err := f.puller.Pull(ctx, second); err != nil || f.remote.calls != 2 {
		t.Fatal("retry downloaded again", f.remote.calls, err)
	}
	for _, prior := range []journal.Proposal{second, folder} {
		deletion := f.proposal("delete-"+prior.Entries[0].Path, prior.Entries[0].Path, prior.Entries[0].Next.ID, nil)
		deletion.Entries[0].Next.Tombstone = true
		deletion.Entries[0].Next.Digest = hash.Digest{}
		if _, err := f.puller.Pull(ctx, deletion); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "folder")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if f.remote.calls != 2 {
		t.Fatal("metadata operations downloaded content")
	}
}

func TestPullLateFailureAndLocalEditPreservation(t *testing.T) {
	f := setup(t)
	p := f.proposal("put", "file", "", []byte("remote"))
	f.remote.fail = true
	if _, err := f.puller.Pull(context.Background(), p); err == nil {
		t.Fatal("late error accepted")
	}
	if _, err := os.Stat(filepath.Join(f.root, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed download published", err)
	}
	if ready, err := f.store.OpenReady(p.Entries[0].Next.ID); err == nil {
		ready.Close()
		t.Fatal("failed download marked ready")
	}
	f.remote.fail = false
	if _, err := f.puller.Pull(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "file"), []byte("local edit"), 0600); err != nil {
		t.Fatal(err)
	}
	next := f.proposal("edit", "file", p.Entries[0].Next.ID, []byte("remote edit"))
	if _, err := f.puller.Pull(context.Background(), next); !errors.Is(err, publish.ErrExternal) {
		t.Fatal("local edit overwritten", err)
	}
	actual, err := os.ReadFile(filepath.Join(f.root, "file"))
	if err != nil || string(actual) != "local edit" {
		t.Fatal(string(actual), err)
	}
}

type failOnce struct {
	p      journal.Publisher
	failed bool
}

func (f *failOnce) Publish(ctx context.Context, r journal.Record) error {
	if err := f.p.Publish(ctx, r); err != nil {
		return err
	}
	if !f.failed {
		f.failed = true
		return errors.New("interrupted before journal commit")
	}
	return nil
}
func TestPullRecoversPublicationWithoutRedownload(t *testing.T) {
	f := setup(t)
	f.puller.publisher = &failOnce{p: f.publisher}
	p := f.proposal("recover", "file", "", []byte("content"))
	if _, err := f.puller.Pull(context.Background(), p); err == nil {
		t.Fatal("injected failure absent")
	}
	result, err := f.puller.Pull(context.Background(), p)
	if err != nil || !result.Record.Committed || f.remote.calls != 1 {
		t.Fatal(result, f.remote.calls, err)
	}
}

func TestPullGatesAndCancellation(t *testing.T) {
	f := setup(t)
	p := f.proposal("gate", "file", "", []byte("data"))
	f.puller.opts.Writes = false
	if _, err := f.puller.Pull(context.Background(), p); err == nil {
		t.Fatal("write gate bypassed")
	}
	f.puller.opts.Writes = true
	f.puller.opts.Gate = func(context.Context) error { return errors.New("off LAN") }
	if _, err := f.puller.Pull(context.Background(), p); err == nil {
		t.Fatal("LAN gate bypassed")
	}
	if f.remote.calls != 0 {
		t.Fatal("closed gate caused download")
	}
	ctx, cancel := context.WithCancel(context.Background())
	pace := newPacer(ctx, 1024)
	pace.bytes = 65536
	cancel()
	start := time.Now()
	if err := pace.before(); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatal("pacing ignored cancellation", err)
	}
}

func TestPullBatchRetryReusesOnlyVerifiedCandidates(t *testing.T) {
	f := setup(t)
	a := f.proposal("batch-a", "a", "", []byte("first"))
	b := f.proposal("batch-b", "b", "", []byte("second"))
	a.Entries = append(a.Entries, b.Entries...)
	f.remote.failOn = 2
	if _, err := f.puller.Pull(context.Background(), a); err == nil {
		t.Fatal("expected second transfer failure")
	}
	if _, err := os.Stat(filepath.Join(f.root, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial staging batch published", err)
	}
	if prior, err := f.c.Operation(a.ID); err != nil || prior != nil {
		t.Fatal("incomplete staging prepared publication", prior, err)
	}
	result, err := f.puller.Pull(context.Background(), a)
	if err != nil || !result.Record.Committed || f.remote.calls != 3 {
		t.Fatal("verified first candidate redownloaded", result, f.remote.calls, err)
	}
	for path, want := range map[string]string{"a": "first", "b": "second"} {
		data, err := os.ReadFile(filepath.Join(f.root, path))
		if err != nil || string(data) != want {
			t.Fatal(path, string(data), err)
		}
	}
}

func TestRecoveryPublisherCannotApplyAnotherOperation(t *testing.T) {
	f := setup(t)
	want := f.proposal("intended", "file", "", []byte("one"))
	other := f.proposal("unrelated", "other", "", []byte("two"))
	wrapper := gatedPublisher{inner: f.publisher, gate: func(context.Context) error { t.Fatal("wrong recovery reached policy gate"); return nil }, want: want}
	if err := wrapper.Publish(context.Background(), journal.Record{Proposal: other}); !errors.Is(err, journal.ErrIdentity) {
		t.Fatal(err)
	}
}
