package stage

import (
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/hash"
)

func TestAbortUploadSealingWindowAndUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := hash.SumBytes([]byte("reserved candidate")).Hex()
	partial := filepath.Join(dir, id+".partial")
	if err := os.WriteFile(partial, []byte("snapshot"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(partial, filepath.Join(dir, id+".ready")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".wire.partial"), []byte("wire"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "unrelated")
	if err := os.WriteFile(unrelated, []byte("keep"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireUploadVacant(id); err == nil {
		t.Fatal("existing candidate claimed")
	}
	if err := s.AbortUncommittedUpload(id); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireUploadVacant(id); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortUncommittedUpload(id); err != nil {
		t.Fatal("repeat cleanup failed", err)
	}
	if b, err := os.ReadFile(unrelated); err != nil || string(b) != "keep" {
		t.Fatal("unrelated file changed", err)
	}
}

func TestAbortUploadRejectsUnownedLinks(t *testing.T) {
	for _, link := range []string{"hard", "symbolic"} {
		t.Run(link, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id := hash.SumBytes([]byte(link)).Hex()
			keep := filepath.Join(dir, "keep")
			ready := filepath.Join(dir, id+".ready")
			if err := os.WriteFile(keep, []byte("preserve"), 0400); err != nil {
				t.Fatal(err)
			}
			if link == "hard" {
				err = os.Link(keep, ready)
			} else {
				err = os.Symlink(keep, ready)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AbortUncommittedUpload(id); err == nil {
				t.Fatal("unowned link accepted")
			}
			if _, err := os.Lstat(ready); err != nil {
				t.Fatal("refused candidate removed", err)
			}
			if b, err := os.ReadFile(keep); err != nil || string(b) != "preserve" {
				t.Fatal("unowned file changed", err)
			}
		})
	}
}
