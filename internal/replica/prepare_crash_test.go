package replica

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"nas-sync/internal/content"
	"nas-sync/internal/index"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

func TestKilledUploadPreparationCanAbortWithoutLosingSource(t *testing.T) {
	if dir := os.Getenv("ANANAS_PREPARATION_CRASH_TEST"); dir != "" {
		rootPath, state := filepath.Join(dir, "root"), filepath.Join(dir, "state")
		c, err := journal.Open(state, id("crash local namespace"))
		if err != nil {
			t.Fatal(err)
		}
		db, err := index.Open(filepath.Join(state, "index.db"), rootPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.ConfigureReplicaBudget(journal.PublicationBudget{MaxBytes: 1 << 20, MaxEntries: 4}, id("remote namespace")); err != nil {
			t.Fatal(err)
		}
		store, err := stage.Open(state)
		if err != nil {
			t.Fatal(err)
		}
		root, err := content.OpenRoot(rootPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		observeFile(t, db, rootPath, "file", []byte("original source"))
		o := completionOptions()
		o.Gate = func(context.Context) error {
			p, err := db.PendingUploadPreparation()
			if err != nil {
				return err
			}
			if p != nil {
				f, err := store.OpenReady(p.Sources[0].ID)
				if err == nil {
					f.Close()
					fmt.Fprintln(os.Stdout, "prepared-snapshot")
					time.Sleep(time.Hour)
					return fmt.Errorf("fixture was not killed")
				}
			}
			return nil
		}
		_, _, err = PrepareUpload(context.Background(), db, root, c, store, &comparisonRemote{}, []string{"file"}, id("remote namespace"), o)
		t.Fatal("crash fixture returned", err)
	}
	dir := t.TempDir()
	rootPath, state := filepath.Join(dir, "root"), filepath.Join(dir, "state")
	for _, p := range []string{rootPath, state} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKilledUploadPreparationCanAbortWithoutLosingSource$")
	cmd.Env = append(os.Environ(), "ANANAS_PREPARATION_CRASH_TEST="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "prepared-snapshot\n" {
		t.Fatal(line, err)
	}
	if other, err := journal.Open(state, id("crash local namespace")); err == nil {
		other.Close()
		t.Fatal("live journal ownership stolen")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal("fixture was not live", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("fixture exited successfully before kill")
	}
	c, err := journal.Open(state, id("crash local namespace"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	beforeBudget, err := c.PublicationBudgetUsage()
	if err != nil || beforeBudget == nil || beforeBudget.Entries != 1 || beforeBudget.ReservedBytes <= 192<<10 {
		t.Fatal("killed snapshot lost reservation", beforeBudget, err)
	}
	db, err := index.Open(filepath.Join(state, "index.db"), rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := stage.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p, err := db.PendingUploadPreparation()
	if err != nil || p == nil {
		t.Fatal("killed preparation lost provenance", p, err)
	}
	if u, err := db.PendingUpload(); err != nil || u != nil {
		t.Fatal("incomplete snapshot became sendable", u, err)
	}
	f, err := store.OpenReady(p.Sources[0].ID)
	if err != nil {
		t.Fatal("killed snapshot not retained", err)
	}
	f.Close()
	if err := AbortUploadPreparation(ctx, db, c, store, p.Namespace); err != nil {
		t.Fatal(err)
	}
	if u, err := c.PublicationBudgetUsage(); err != nil || u == nil || u.ReservedBytes != 0 || u.Entries != 0 {
		t.Fatal("verified abort did not release cache", u, err)
	}
	if err := store.RequireUploadVacant(p.Sources[0].ID); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(rootPath, "file")); err != nil || string(b) != "original source" {
		t.Fatal("source changed during recovery", err)
	}
	if records, err := db.DirtyPage("", 1); err != nil || len(records) != 1 || !records[0].Dirty {
		t.Fatal("queued source lost", records, err)
	}
}
