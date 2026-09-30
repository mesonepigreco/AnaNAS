package stage

import (
	"os"
	"path/filepath"
	"testing"
)

func bootstrapStore(t *testing.T, names ...string) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(path, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestBudgetBootstrapAcceptsAReplicasOwnStateFiles(t *testing.T) {
	// The daemon opens its transfer history before synchronization starts, so the
	// history sits beside the journal and index by the time the budget is set up.
	for _, names := range [][]string{
		{"journal.db"},
		{"journal.db", "index.db"},
		{"journal.db", "traffic.db"},
		{"journal.db", "index.db", "traffic.db"},
	} {
		store, _ := bootstrapStore(t, names...)
		if err := store.RequireBudgetBootstrap(true); err != nil {
			t.Fatalf("replica bootstrap with %v: %v", names, err)
		}
	}
}

func TestBudgetBootstrapRefusesContentItDoesNotAccountFor(t *testing.T) {
	for _, names := range [][]string{
		{"journal.db", "spool"},
		{"journal.db", "index.db", "traffic.db", "leftover.db"},
	} {
		store, _ := bootstrapStore(t, names...)
		if err := store.RequireBudgetBootstrap(true); err == nil {
			t.Fatalf("replica bootstrap with %v: accepted unaccounted state", names)
		}
	}
	// A receiver keeps only its journal; the replica's files are not its own.
	for _, names := range [][]string{{"journal.db", "index.db"}, {"journal.db", "traffic.db"}} {
		store, _ := bootstrapStore(t, names...)
		if err := store.RequireBudgetBootstrap(false); err == nil {
			t.Fatalf("receiver bootstrap with %v: accepted replica state", names)
		}
	}
}

func TestBudgetBootstrapRequiresItsJournal(t *testing.T) {
	for _, replica := range []bool{true, false} {
		store, _ := bootstrapStore(t)
		if err := store.RequireBudgetBootstrap(replica); err == nil {
			t.Fatal("empty state accepted without a journal")
		}
	}
	store, _ := bootstrapStore(t, "index.db", "traffic.db")
	if err := store.RequireBudgetBootstrap(true); err == nil {
		t.Fatal("missing journal accepted")
	}
}

func TestBudgetBootstrapRefusesGroupReadablePrivateState(t *testing.T) {
	s, path := bootstrapStore(t, "journal.db", "index.db")
	if err := os.Chmod(filepath.Join(path, "index.db"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireBudgetBootstrap(true); err == nil {
		t.Fatal("group-readable index accepted")
	}
}
