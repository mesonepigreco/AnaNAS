package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplicaBudgetBootstrapChecksObserverIndexMetadata(t *testing.T) {
	for _, kind := range []string{"private-index", "public-index", "symlink-index", "linked-index", "directory-index", "extra-payload"} {
		t.Run(kind, func(t *testing.T) {
			c, dir := openTest(t)
			index := filepath.Join(dir, "index.db")
			switch kind {
			case "directory-index":
				if err := os.Mkdir(index, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink-index":
				if err := os.Symlink("journal.db", index); err != nil {
					t.Fatal(err)
				}
			case "linked-index":
				if err := os.Link(filepath.Join(dir, "journal.db"), index); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(index, []byte("metadata only"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "public-index" {
					if err := os.Chmod(index, 0644); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "extra-payload" {
					if err := os.WriteFile(filepath.Join(dir, id(4)+".ready"), []byte("keep"), 0400); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := c.ConfigureReplicaBudget(PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}, id(77))
			if kind == "private-index" {
				if err != nil {
					t.Fatal("private observer metadata refused", err)
				}
			} else if err == nil {
				t.Fatal("unaccounted or unsafe index accepted")
			}
			if _, err := os.Lstat(index); err != nil {
				t.Fatal("bootstrap removed existing state", err)
			}
		})
	}
}
