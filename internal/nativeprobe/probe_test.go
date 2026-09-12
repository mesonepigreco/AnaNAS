package nativeprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func parent(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nas-sync-capability-test")
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "sentinel"), []byte("existing disposable data untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func intact(t *testing.T, p string) {
	t.Helper()
	entries, err := os.ReadDir(p)
	if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatal("probe left files or removed prior data", entries, err)
	}
	got, err := os.ReadFile(filepath.Join(p, "sentinel"))
	if err != nil || string(got) != "existing disposable data untouched" {
		t.Fatal("sentinel changed", err)
	}
}
func TestReadOnlyAndWriteProbe(t *testing.T) {
	p := parent(t)
	r, err := Run(context.Background(), p, false)
	if err != nil || r.Write || r.Cleaned || len(r.Checks) != 1 {
		t.Fatal(r, err)
	}
	intact(t, p)
	r, err = Run(context.Background(), p, true)
	if err != nil || !r.Write || !r.Cleaned || r.LiteralBytes != 1 || r.ReusedBytes != FixtureBytes || r.DeltaBytes != 491 || r.SignatureBytes != 344 || len(r.Checks) != 11 {
		t.Fatal(r, err)
	}
	intact(t, p)
}
func TestRejectScopeAndCancellation(t *testing.T) {
	p := parent(t)
	for _, bad := range []string{"", ".", filepath.Dir(p), filepath.Join(p, "missing", "nas-sync-capability-test")} {
		if _, err := Run(context.Background(), bad, true); err == nil {
			t.Fatal("unsafe path accepted", bad)
		}
	}
	link := filepath.Join(t.TempDir(), "nas-sync-capability-test")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), link, true); err == nil {
		t.Fatal("symlink scope accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, p, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	intact(t, p)
}
