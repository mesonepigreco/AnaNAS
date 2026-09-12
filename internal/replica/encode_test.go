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

	"nas-sync/internal/delta"
	"nas-sync/internal/stage"
)

func TestEncodedUploadSurvivesReopenAndRejectsCorruption(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := make([]byte, 256*1024)
	if _, err := rand.Read(base); err != nil {
		t.Fatal(err)
	}
	target := append([]byte{4}, base...)
	p := f.proposal("encoded", "file", "", target)
	v := p.Entries[0].Next
	if err := f.store.ReceiveVerified(ctx, v.ID, v.Size, v.Digest, func(w io.Writer) error { _, err := w.Write(target); return err }); err != nil {
		t.Fatal(err)
	}
	sig, err := delta.Build(ctx, bytes.NewReader(base), int64(len(base)), 65536)
	if err != nil {
		t.Fatal(err)
	}
	info, stats, err := EncodeUpload(ctx, f.store, v, sig, 1<<30)
	if err != nil || stats.LiteralBytes != 1 || stats.ReusedBytes != int64(len(base)) || info.Size >= int64(len(base)) {
		t.Fatal(info, stats, err)
	}
	reopened, err := stage.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	wire, err := OpenUploadWire(ctx, reopened, v.ID, info, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := delta.Apply(ctx, wire, bytes.NewReader(base), sig.Size, sig.Digest, &out); err != nil || !bytes.Equal(out.Bytes(), target) {
		t.Fatal(err)
	}
	wire.Close()
	if _, _, err := EncodeUpload(ctx, f.store, v, sig, 1<<30); !errors.Is(err, os.ErrExist) {
		t.Fatal("wire was replaced", err)
	}
	path := filepath.Join(f.state, v.ID+".wire.ready")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{0}, int(info.Size)), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if wire, err := OpenUploadWire(ctx, reopened, v.ID, info, 1<<30); err == nil {
		wire.Close()
		t.Fatal("corrupt spool accepted")
	}
}

func TestEncodeRejectsChangedSnapshotWithoutReadyWire(t *testing.T) {
	f := setup(t)
	p := f.proposal("changed-snapshot", "file", "", []byte("good"))
	v := p.Entries[0].Next
	if _, err := f.store.Capture(context.Background(), v.ID, 4, func(w io.Writer) error { _, err := io.WriteString(w, "evil"); return err }); err != nil {
		t.Fatal(err)
	}
	sig, err := delta.Build(context.Background(), bytes.NewReader(nil), 0, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := EncodeUpload(context.Background(), f.store, v, sig, 1<<30); err == nil {
		t.Fatal("wrong snapshot encoded")
	}
	if file, err := f.store.OpenWire(v.ID); err == nil {
		file.Close()
		t.Fatal("failed encoding sealed")
	}
	if _, err := os.Stat(filepath.Join(f.state, v.ID+".wire.partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
