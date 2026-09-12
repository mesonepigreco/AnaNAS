package publish

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
	"testing"
	"time"

	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

func id(n int) string { return fmt.Sprintf("%064x", n) }

type fixture struct {
	root, state string
	c           *journal.Coordinator
	p           *Publisher
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{root: filepath.Join(dir, "root"), state: filepath.Join(dir, "state")}
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.state, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	f.c, err = journal.Open(f.state, id(99))
	if err != nil {
		t.Fatal(err)
	}
	f.p, err = Open(f.root, f.state, f.c.Version, nil, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.p.Close(); f.c.Close() })
	return f
}
func candidate(t *testing.T, state string, n int, data []byte) journal.Version {
	t.Helper()
	s, err := stage.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base, err := delta.Build(context.Background(), bytes.NewReader(nil), 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if _, err := delta.Encode(context.Background(), bytes.NewReader(data), int64(len(data)), base, &wire); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Receive(context.Background(), id(n), &wire, nil, 0, hash.SumBytes(nil)); err != nil {
		t.Fatal(err)
	}
	return journal.Version{ID: id(n), Size: int64(len(data)), Digest: hash.SumBytes(data)}
}
func proposal(n int, path, expected string, v journal.Version) journal.Proposal {
	return journal.Proposal{ID: id(n + 100), Client: id(1), Entries: []journal.Entry{{Path: path, Expected: expected, Next: v}}}
}
func commit(t *testing.T, f *fixture, n int, path, expected string, v journal.Version) {
	t.Helper()
	r, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(n, path, expected, v), f.p)
	if err != nil || !r.Committed {
		t.Fatal(r, err)
	}
}
func contents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s: got %q want %q: %v", path, got, want, err)
	}
}

func TestCreateReplaceDeleteAndRetention(t *testing.T) {
	f := setup(t)
	old := []byte("original content")
	next := []byte("edited content")
	v1 := candidate(t, f.state, 2, old)
	commit(t, f, 2, "file", "", v1)
	contents(t, filepath.Join(f.root, "file"), old)
	v2 := candidate(t, f.state, 3, next)
	commit(t, f, 3, "file", v1.ID, v2)
	contents(t, filepath.Join(f.root, "file"), next)
	contents(t, filepath.Join(f.state, v2.ID+".publish"), old)
	contents(t, filepath.Join(f.state, v1.ID+".ready"), old)
	contents(t, filepath.Join(f.state, v2.ID+".ready"), next)
	// Editing the visible inode must not mutate the retained immutable version.
	if err := os.WriteFile(filepath.Join(f.root, "file"), []byte("external"), 0600); err != nil {
		t.Fatal(err)
	}
	contents(t, filepath.Join(f.state, v2.ID+".ready"), next)
	// Restore the expected version to exercise an intentional delete.
	if err := os.WriteFile(filepath.Join(f.root, "file"), next, 0600); err != nil {
		t.Fatal(err)
	}
	tombstone := journal.Version{ID: id(4), Tombstone: true}
	commit(t, f, 4, "file", v2.ID, tombstone)
	if _, err := os.Lstat(filepath.Join(f.root, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("delete did not remove visible name", err)
	}
	contents(t, filepath.Join(f.state, tombstone.ID+".displaced"), next)
	if head, pending, err := f.c.Head("file"); err != nil || pending || head == nil || !head.Tombstone {
		t.Fatal(head, pending, err)
	}
}

func TestNestedParentsAndExclusions(t *testing.T) {
	f := setup(t)
	if err := os.MkdirAll(filepath.Join(f.root, "a/b"), 0700); err != nil {
		t.Fatal(err)
	}
	v := candidate(t, f.state, 2, []byte("nested"))
	commit(t, f, 2, "a/b/file", "", v)
	contents(t, filepath.Join(f.root, "a/b/file"), []byte("nested"))
	f.p.Close()
	var err error
	f.p, err = Open(f.root, f.state, f.c.Version, []string{"a/"}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	v = candidate(t, f.state, 3, []byte("excluded"))
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(3, "a/other", "", v), f.p); err == nil {
		t.Fatal("published excluded path")
	}
	if _, err := os.Lstat(filepath.Join(f.root, "a/other")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestExternalEditPreservedBeforePublish(t *testing.T) {
	f := setup(t)
	v1 := candidate(t, f.state, 2, []byte("old"))
	commit(t, f, 2, "file", "", v1)
	if err := os.WriteFile(filepath.Join(f.root, "file"), []byte("outside edit"), 0600); err != nil {
		t.Fatal(err)
	}
	v2 := candidate(t, f.state, 3, []byte("incoming"))
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(3, "file", v1.ID, v2), f.p); !errors.Is(err, ErrExternal) {
		t.Fatal(err)
	}
	contents(t, filepath.Join(f.root, "file"), []byte("outside edit"))
	contents(t, filepath.Join(f.state, v2.ID+".ready"), []byte("incoming"))
	if _, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); !errors.Is(err, ErrExternal) {
		t.Fatal("retry forgot external conflict", err)
	}
}

func TestRacingWriterRetainedAcrossRecovery(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		t.Run(fmt.Sprint(deletion), func(t *testing.T) {
			f := setup(t)
			v1 := candidate(t, f.state, 2, []byte("old"))
			commit(t, f, 2, "file", "", v1)
			writer, err := os.OpenFile(filepath.Join(f.root, "file"), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			v2 := journal.Version{ID: id(3), Tombstone: true}
			suffix := ".displaced"
			if !deletion {
				v2 = candidate(t, f.state, 3, []byte("incoming"))
				suffix = ".publish"
			}
			f.p.afterRename = func() error { _, err := writer.WriteAt([]byte("late external writer"), 0); return err }
			if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(3, "file", v1.ID, v2), f.p); !errors.Is(err, ErrExternal) {
				t.Fatal(err)
			}
			f.p.afterRename = nil
			contents(t, filepath.Join(f.state, v2.ID+suffix), []byte("late external writer"))
			for i := 0; i < 2; i++ {
				if _, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); !errors.Is(err, ErrExternal) {
					t.Fatal("recovery silently cleared conflict", err)
				}
			}
			if !deletion {
				contents(t, filepath.Join(f.root, "file"), []byte("incoming"))
			}
			if _, pending, err := f.c.Head("file"); err != nil || !pending {
				t.Fatal("conflicting operation committed", pending, err)
			}
		})
	}
}

func TestNoReplaceRace(t *testing.T) {
	f := setup(t)
	v := candidate(t, f.state, 2, []byte("incoming"))
	f.p.beforeRename = func() error { return os.WriteFile(filepath.Join(f.root, "file"), []byte("racing create"), 0600) }
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(2, "file", "", v), f.p); err == nil {
		t.Fatal("overwrote racing create")
	}
	contents(t, filepath.Join(f.root, "file"), []byte("racing create"))
	contents(t, filepath.Join(f.state, v.ID+".publish"), []byte("incoming"))
}

func TestRejectSymlinksMissingParentsAndTamperedCandidate(t *testing.T) {
	for _, kind := range []string{"symlink-file", "symlink-parent", "missing-parent", "tampered"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			v := candidate(t, f.state, 2, []byte("incoming"))
			path := "file"
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink-file":
				if err := os.Symlink(outside, filepath.Join(f.root, path)); err != nil {
					t.Fatal(err)
				}
			case "symlink-parent":
				if err := os.Symlink(filepath.Dir(outside), filepath.Join(f.root, "parent")); err != nil {
					t.Fatal(err)
				}
				path = "parent/file"
			case "missing-parent":
				path = "missing/file"
			case "tampered":
				name := filepath.Join(f.state, v.ID+".ready")
				if err := os.Chmod(name, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte("corrupt"), 0400); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(name, 0400); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(2, path, "", v), f.p); err == nil {
				t.Fatal("unsafe publication accepted")
			}
			contents(t, outside, []byte("untouched"))
		})
	}
}

func TestInterruptedCopyRetryAndCanceledPublication(t *testing.T) {
	f := setup(t)
	v := candidate(t, f.state, 2, []byte("incoming"))
	if err := os.WriteFile(filepath.Join(f.state, v.ID+".copying"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	commit(t, f, 2, "file", "", v)
	contents(t, filepath.Join(f.root, "file"), []byte("incoming"))
	v2 := candidate(t, f.state, 3, []byte("next"))
	ctx, cancel := context.WithCancel(context.Background())
	f.p.afterRename = func() error { cancel(); return context.Canceled }
	if _, err := f.c.Commit(ctx, f.c.Epoch(), proposal(3, "file", v.ID, v2), f.p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.p.afterRename = nil
	if r, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); err != nil || r == nil || !r.Committed {
		t.Fatal(r, err)
	}
	contents(t, filepath.Join(f.root, "file"), []byte("next"))
}

func TestKilledPublisherAfterNamespaceChange(t *testing.T) {
	if root := os.Getenv("ANANAS_PUBLISH_CRASH_ROOT"); root != "" {
		state := os.Getenv("ANANAS_PUBLISH_CRASH_STATE")
		c, err := journal.Open(state, id(99))
		if err != nil {
			t.Fatal(err)
		}
		p, err := Open(root, state, c.Version, nil, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		p.afterRename = func() error {
			fmt.Fprintln(os.Stdout, "renamed")
			for {
				time.Sleep(time.Hour)
			}
		}
		v := journal.Version{ID: id(3), Size: 8, Digest: hash.SumBytes([]byte("incoming"))}
		_, err = c.Commit(context.Background(), c.Epoch(), proposal(3, "file", id(2), v), p)
		t.Fatal("unexpected child return", err)
	}
	f := setup(t)
	v1 := candidate(t, f.state, 2, []byte("old"))
	commit(t, f, 2, "file", "", v1)
	candidate(t, f.state, 3, []byte("incoming"))
	f.c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKilledPublisherAfterNamespaceChange$")
	cmd.Env = append(os.Environ(), "ANANAS_PUBLISH_CRASH_ROOT="+f.root, "ANANAS_PUBLISH_CRASH_STATE="+f.state)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	cmd.Process.Kill()
	cmd.Wait()
	if err != nil || line != "renamed\n" {
		t.Fatal(line, err)
	}
	contents(t, filepath.Join(f.root, "file"), []byte("incoming"))
	contents(t, filepath.Join(f.state, id(3)+".publish"), []byte("old"))
	f.c, err = journal.Open(f.state, id(99))
	if err != nil {
		t.Fatal(err)
	}
	f.p.Close()
	f.p, err = Open(f.root, f.state, f.c.Version, nil, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); err != nil || r == nil || !r.Committed {
		t.Fatal(r, err)
	}
	if page, err := f.c.Changes(0, 4); err != nil || len(page) != 2 {
		t.Fatal(page, err)
	}
	contents(t, filepath.Join(f.state, id(2)+".ready"), []byte("old"))
}

func TestStableHashIgnoresReadAtime(t *testing.T) {
	f := setup(t)
	name := filepath.Join(f.root, "file")
	if err := os.WriteFile(name, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, time.Unix(1, 0), time.Now()); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var copied bytes.Buffer
	d, n, err := f.p.digestCopy(context.Background(), file, &copied)
	if err != nil || d != hash.SumBytes([]byte("content")) || n != 7 {
		t.Fatal(d, n, err)
	}
	if _, err := io.Copy(io.Discard, &copied); err != nil {
		t.Fatal(err)
	}
}

func TestExternalDeleteAfterCreateIsNotUndoneOnRecovery(t *testing.T) {
	f := setup(t)
	v := candidate(t, f.state, 2, []byte("incoming"))
	f.p.afterRename = func() error {
		if err := os.Remove(filepath.Join(f.root, "file")); err != nil {
			return err
		}
		return context.Canceled
	}
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), proposal(2, "file", "", v), f.p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.p.afterRename = nil
	if _, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); !errors.Is(err, ErrExternal) {
		t.Fatal("recovery silently recreated externally deleted file", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	contents(t, filepath.Join(f.state, v.ID+".ready"), []byte("incoming"))
}

func TestDeltaThroughDurablePublication(t *testing.T) {
	f := setup(t)
	old := make([]byte, 8*64*1024)
	for i := range old {
		old[i] = byte(i*31 + i/1024)
	}
	v1 := candidate(t, f.state, 2, old)
	commit(t, f, 2, "file", "", v1)
	next := append([]byte{99}, old...)
	s, err := stage.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base, err := s.OpenReady(v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	sig, err := delta.Build(context.Background(), base, int64(len(old)), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	stats, err := delta.Encode(context.Background(), bytes.NewReader(next), int64(len(next)), sig, &wire)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LiteralBytes != 1 || stats.ReusedBytes != int64(len(old)) || wire.Len() > 1024 {
		t.Fatal("unchanged payload crossed delta stream", stats, wire.Len())
	}
	if _, err := s.Receive(context.Background(), id(3), &wire, base, v1.Size, v1.Digest); err != nil {
		t.Fatal(err)
	}
	v2 := journal.Version{ID: id(3), Size: int64(len(next)), Digest: hash.SumBytes(next)}
	commit(t, f, 3, "file", v1.ID, v2)
	contents(t, filepath.Join(f.root, "file"), next)
	contents(t, filepath.Join(f.state, v1.ID+".ready"), old)
	if err := f.c.Acknowledge(id(1), 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Acknowledge(id(1), 1, 2); err != nil {
		t.Fatal(err)
	}
}

func TestPartlyPublishedBatchRecoversWithoutDuplicateSequence(t *testing.T) {
	f := setup(t)
	a := candidate(t, f.state, 2, []byte("first"))
	b := candidate(t, f.state, 3, []byte("second"))
	p := proposal(2, "first", "", a)
	p.Entries = append(p.Entries, journal.Entry{Path: "second", Next: b})
	f.p.afterRename = func() error { return context.Canceled }
	if _, err := f.c.Commit(context.Background(), f.c.Epoch(), p, f.p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	contents(t, filepath.Join(f.root, "first"), []byte("first"))
	if _, err := os.Lstat(filepath.Join(f.root, "second")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if changes, err := f.c.Changes(0, 4); err != nil || len(changes) != 0 {
		t.Fatal("partial batch appeared committed", changes, err)
	}
	f.p.afterRename = nil
	if r, err := f.c.Recover(context.Background(), f.c.Epoch(), f.p); err != nil || r == nil || !r.Committed {
		t.Fatal(r, err)
	}
	contents(t, filepath.Join(f.root, "second"), []byte("second"))
	if changes, err := f.c.Changes(0, 4); err != nil || len(changes) != 1 || len(changes[0].Entries) != 2 {
		t.Fatal(changes, err)
	}
}
