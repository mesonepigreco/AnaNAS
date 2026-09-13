// Legacy archive template: replace illustrative fixture names and hashes before use.
// Stop nas-sync.service before --apply so the database lock is exclusive.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"nas-sync/internal/config"
	"nas-sync/internal/index"
	"nas-sync/internal/languard"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

const local = "/home/example-user/NASdir"
const nas = "/mnt/nasdir"

var names = []string{"anaNAS-live-check.bin", "anaNAS-folders-aaaaaaaaaaaa", "anaNAS-deletions-bbbbbbbbbbbb"}
var dirs = []string{"anaNAS-folders-aaaaaaaaaaaa", "anaNAS-folders-aaaaaaaaaaaa/inner", "anaNAS-folders-aaaaaaaaaaaa/empty", "anaNAS-deletions-bbbbbbbbbbbb"}
var hashes = map[string]string{
	"anaNAS-live-check.bin":                       "1111111111111111111111111111111111111111111111111111111111111111",
	"anaNAS-folders-aaaaaaaaaaaa/inner/probe.bin": "2222222222222222222222222222222222222222222222222222222222222222",
}

func check(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive root is not a native directory")
	}
	var actual []string
	for _, name := range names {
		err := filepath.WalkDir(filepath.Join(root, name), func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			rel, _ := filepath.Rel(root, p)
			actual = append(actual, rel)
			if d.IsDir() {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink: %s", p)
			}
			expected, ok := hashes[rel]
			if !ok {
				return fmt.Errorf("unexpected fixture file: %s", p)
			}
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			defer f.Close()
			h := sha256.New()
			if _, e = io.Copy(h, f); e != nil {
				return e
			}
			if fmt.Sprintf("%x", h.Sum(nil)) != expected {
				return fmt.Errorf("fixture changed: %s", p)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	expected := append([]string{}, dirs...)
	for p := range hashes {
		expected = append(expected, p)
	}
	sort.Strings(expected)
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("fixture tree differs: %v", actual)
	}
	return nil
}
func run(apply, finish bool) error {
	raw, e := os.ReadFile("/proc/self/mountinfo")
	if e != nil {
		return e
	}
	mounts, e := languard.ParseMounts(raw)
	if e != nil {
		return e
	}
	if !languard.EvaluateMount(config.NAS{Host: "10.23.42.30", Share: "Nasdir", MountPoint: nas, Protocol: "smb"}, mounts).Eligible {
		return fmt.Errorf("expected NAS mount unavailable")
	}
	if finish {
		for _, root := range []string{local, nas} {
			if e = check(filepath.Join(root, ".ananas-tests")); e != nil {
				return e
			}
			for _, name := range names {
				if _, e = os.Lstat(filepath.Join(root, name)); !os.IsNotExist(e) {
					return fmt.Errorf("original fixture path still exists or cannot be inspected")
				}
			}
		}
		db, e := index.Open("/home/example-user/.local/state/nas-sync/nasdir/index.db", local)
		if e != nil {
			return e
		}
		defer db.Close()
		// Include already-deleted startup probes from these exact fixture trees.
		// They are absent from a filesystem listing but still have indexed history.
		var paths []string
		for _, name := range names {
			rows, e := db.PagePrefix(name+"/", "", 1000)
			if e != nil {
				return e
			}
			if len(rows) == 1000 {
				return fmt.Errorf("unexpectedly large fixture history")
			}
			row, ok, e := db.Get(name)
			if e != nil {
				return e
			}
			if ok {
				rows = append(rows, row)
			}
			for _, r := range rows {
				if !os.FileMode(r.Fingerprint.Mode).IsDir() {
					paths = append(paths, r.Path)
				}
			}
		}
		if e = db.RetireArchivedTombstones(paths); e != nil {
			return e
		}
		fmt.Printf("Verified both archives and retired %d acknowledged obsolete file tombstones.\n", len(paths))
		return nil
	}
	for _, root := range []string{local, nas} {
		if e = check(root); e != nil {
			return e
		}
		if _, e = os.Lstat(filepath.Join(root, ".ananas-tests")); !os.IsNotExist(e) {
			return fmt.Errorf("archive destination already exists or cannot be inspected")
		}
	}
	fmt.Println("Both fixture trees and hashes verified; archive destination: .ananas-tests")
	if !apply {
		return nil
	}
	db, e := index.Open("/home/example-user/.local/state/nas-sync/nasdir/index.db", local)
	if e != nil {
		return e
	}
	defer db.Close()
	// Preserve directory identities/history while retiring the obsolete paths.
	if e = db.RetireArchivedDirectories(dirs); e != nil {
		return e
	}
	dest := filepath.Join(local, ".ananas-tests")
	if e = os.Mkdir(dest, 0700); e != nil {
		return e
	}
	for _, name := range names {
		if e = os.Rename(filepath.Join(local, name), filepath.Join(dest, name)); e != nil {
			return e
		}
	}
	// The current file-deletion receipt requires an accessible parent. Leave
	// empty retired parents until file tombstones and new archive copies converge.
	for _, path := range dirs {
		if e = os.MkdirAll(filepath.Join(local, path), 0700); e != nil {
			return e
		}
	}
	fmt.Println("Local fixtures archived. Start the daemon, verify hidden NAS copies and original-file deletions, then remove the empty original directories on both sides.")
	return nil
}
func main() {
	if os.Getenv("ANANAS_LEGACY_PROFILE_CONFIGURED") != "yes" {
		fmt.Fprintln(os.Stderr, "Historical fixture maintenance template: configure and review targets before explicit opt-in.")
		os.Exit(2)
	}
	apply := flag.Bool("apply", false, "archive the fixed verified trial fixtures")
	finish := flag.Bool("finish", false, "retire acknowledged original-file tombstones after archive verification")
	inspect := flag.String("inspect-index", "", "inspect a copied local index without filesystem changes")
	flag.Parse()
	if *inspect != "" {
		db, e := index.Open(*inspect, local)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		defer db.Close()
		pending, e := db.PendingUpload()
		fmt.Println("upload", pending, e)
		prep, e := db.PendingUploadPreparation()
		fmt.Println("preparation", prep, e)
		remote, e := db.RemoteState()
		fmt.Println("remote", remote, e)
		batch, e := db.NextRemoteBatch()
		fmt.Println("next batch", batch, e)
		rows, e := db.DirtyPage("", 1000)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		for _, row := range rows {
			state, e := db.PathState(row.Path)
			if e != nil {
				panic(e)
			}
			json.NewEncoder(os.Stdout).Encode(struct {
				Path            string
				Missing, Paused bool
				Mode            uint32
			}{row.Path, row.Missing, state.Paused, row.Fingerprint.Mode})
		}
		return
	}
	if e := run(*apply, *finish); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
