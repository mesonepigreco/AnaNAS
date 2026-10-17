package replica

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

func TestIncomingConflictContinuesAndRecoveryPreservesLocalCopy(t *testing.T) {
	for _, choice := range []string{"nas", "separate", "retry"} {
		t.Run(choice, func(t *testing.T) {
			f, db, root := preparationFixture(t)
			defer func() { db.Close() }()
			ctx := context.Background()
			f.puller.publisher = incomingPublisher{db: db, c: f.c, inner: f.publisher}
			var seq uint64
			apply := func(p journal.Proposal) {
				t.Helper()
				seq++
				state, _ := db.RemoteState()
				page, err := changefeed.Filter(id("remote namespace"), nil, changefeed.Request{Namespace: state.Namespace, Policy: state.Policy, After: seq - 1, Limit: 1}, []journal.Record{{Proposal: p, Sequence: seq, Epoch: 1, Committed: true}})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.ReceiveRemotePage(page); err != nil {
					t.Fatal(err)
				}
				if _, err := f.puller.Pull(ctx, p); err != nil {
					t.Fatal(err)
				}
				if err := FinishDownload(ctx, db, f.c, f.store, id("remote namespace"), completionOptions()); err != nil {
					t.Fatal(err)
				}
			}
			first := f.proposal("base", "file", "", []byte("old content"))
			apply(first)
			observeFile(t, db, f.root, "file", []byte("my unsent local edit"))
			incoming := f.proposal("remote-edit", "file", first.Entries[0].Next.ID, []byte("NAS update"))
			other := f.proposal("other", "unrelated", "", []byte("continues"))
			incoming.Entries = append(incoming.Entries, other.Entries...)
			apply(incoming)
			if got, _ := os.ReadFile(filepath.Join(f.root, "file")); string(got) != "my unsent local edit" {
				t.Fatal("local edit lost", string(got))
			}
			if got, _ := os.ReadFile(filepath.Join(f.root, "unrelated")); string(got) != "continues" {
				t.Fatal("unrelated publication blocked")
			}
			issue, err := db.Incoming("file")
			if err != nil || issue == nil {
				t.Fatal(issue, err)
			}
			request := id("recovery request")
			if err := db.ChooseIncoming(index.IncomingChoice{Path: issue.Path, Revision: issue.Revision, Choice: choice}, request); err != nil {
				t.Fatal(err)
			}
			// Durable choices survive an index restart.
			db.Close()
			db, err = index.Open(filepath.Join(f.state, "index.db"), f.root)
			if err != nil {
				t.Fatal(err)
			}
			f.puller.publisher = incomingPublisher{db: db, c: f.c, inner: f.publisher}
			w := &Worker{db: db, root: root, c: f.c, store: f.store, puller: f.puller, opts: WorkerOptions{PushOptions: completionOptions(), Ready: func() bool { return true }}}
			if choice == "nas" {
				// Emulate a crash after backup, before restoring any NAS bytes.
				if err := f.publisher.PreserveForResolution(ctx, "file", request); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.resolveIncoming(ctx); err != nil {
				t.Fatal(err)
			}
			issue, err = db.Incoming("file")
			if err != nil {
				t.Fatal(err)
			}
			if choice == "nas" {
				if issue != nil {
					t.Fatal(issue)
				}
				if got, _ := os.ReadFile(filepath.Join(f.root, "file")); string(got) != "NAS update" {
					t.Fatal(string(got))
				}
				if got, _ := os.ReadFile(filepath.Join(f.state, "recovered", "file-"+request)); string(got) != "my unsent local edit" {
					t.Fatal("backup lost", string(got))
				}
			} else {
				if issue == nil {
					t.Fatal("unresolved conflict discarded")
				}
				if got, _ := os.ReadFile(filepath.Join(f.root, "file")); string(got) != "my unsent local edit" {
					t.Fatal("local edit overwritten")
				}
			}
		})
	}
}

func TestIncomingDirectoryRecoveryIncludesNeverPublishedChildren(t *testing.T) {
	f, db, root := preparationFixture(t)
	defer db.Close()
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(f.root, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	observeFile(t, db, f.root, "folder/local-only", []byte("keep this"))
	f.puller.publisher = incomingPublisher{db: db, c: f.c, inner: f.publisher}
	create := journal.Proposal{ID: id("mkdir"), Client: id("remote-client"), Entries: []journal.Entry{{Path: "folder", Next: journal.Version{ID: id("directory"), Directory: true}}}}
	child := f.proposal("child", "folder/file", "", []byte("remote child"))
	sibling := f.proposal("sibling", "folder-other", "", []byte("sibling"))
	page, err := changefeed.Filter(id("remote namespace"), nil, changefeed.Request{Limit: 3}, []journal.Record{{Proposal: create, Sequence: 1, Epoch: 1, Committed: true}, {Proposal: sibling, Sequence: 2, Epoch: 1, Committed: true}, {Proposal: child, Sequence: 3, Epoch: 1, Committed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReceiveRemotePage(page); err != nil {
		t.Fatal(err)
	}
	for _, p := range []journal.Proposal{create, sibling, child} {
		if _, err := f.puller.Pull(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := FinishDownload(ctx, db, f.c, f.store, id("remote namespace"), completionOptions()); err != nil {
			t.Fatal(err)
		}
	}
	issue, _ := db.Incoming("folder")
	if issue == nil {
		t.Fatal("untracked folder silently adopted")
	}
	request := id("directory recovery")
	if err := db.ChooseIncoming(index.IncomingChoice{Path: "folder", Revision: issue.Revision, Choice: "nas"}, request); err != nil {
		t.Fatal(err)
	}
	w := &Worker{db: db, root: root, c: f.c, store: f.store, puller: f.puller, opts: WorkerOptions{PushOptions: completionOptions(), Ready: func() bool { return true }}}
	if _, err := w.resolveIncoming(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "folder/file")); string(got) != "remote child" {
		t.Fatal("never-published child not restored", string(got))
	}
	if got, _ := os.ReadFile(filepath.Join(f.state, "recovered", "folder-"+request, "local-only")); string(got) != "keep this" {
		t.Fatal("local subtree lost")
	}
	problems, err := db.IncomingProblems(ctx, "")
	if err != nil || problems.Total != 0 {
		t.Fatal(problems, err)
	}
}

func TestIncomingRecoversAlreadyPreparedLegacyDirectoryDeletion(t *testing.T) {
	f, db, _ := preparationFixture(t)
	defer db.Close()
	ctx := context.Background()
	dirs := journal.Proposal{ID: id("legacy dirs"), Client: id("remote-client"), Entries: []journal.Entry{{Path: "a", Next: journal.Version{ID: id("dir a"), Directory: true}}, {Path: "b", Next: journal.Version{ID: id("dir b"), Directory: true}}}}
	if _, err := f.puller.Pull(ctx, dirs); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.state, id("dir b")+".directory")); err != nil {
		t.Fatal(err)
	}
	deletes := journal.Proposal{ID: id("legacy delete"), Client: dirs.Client, Entries: []journal.Entry{{Path: "a", Expected: id("dir a"), Next: journal.Version{ID: id("delete a"), Tombstone: true}}, {Path: "b", Expected: id("dir b"), Next: journal.Version{ID: id("delete b"), Tombstone: true}}}}
	if _, err := f.puller.Pull(ctx, deletes); err == nil {
		t.Fatal("missing receipt did not reproduce the production failure")
	}
	if pending, err := f.c.PendingOperation(); err != nil || !pending {
		t.Fatal(pending, err)
	}
	f.puller.publisher = incomingPublisher{db: db, c: f.c, inner: f.publisher}
	if _, err := f.puller.Pull(ctx, deletes); err != nil {
		t.Fatal("prepared deletion remained globally blocked", err)
	}
	if pending, err := f.c.PendingOperation(); err != nil || pending {
		t.Fatal(pending, err)
	}
	if issue, err := db.Incoming("b"); err != nil || issue == nil {
		t.Fatal(issue, err)
	}
	if _, err := f.puller.Pull(ctx, f.proposal("later independent", "independent", "", []byte("still works"))); err != nil {
		t.Fatal(err)
	}
}
