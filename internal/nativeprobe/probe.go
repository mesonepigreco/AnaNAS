// Package nativeprobe exercises the transfer primitives only in a newly created
// child of an explicitly selected, pre-existing disposable test directory.
package nativeprobe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/publish"
	"nas-sync/internal/stage"
)

const FixtureBytes = 512 * 1024
const EstimatedDiskBytes = 8 * 1024 * 1024

type Report struct {
	Scope              string   `json:"scope"`
	TestDir            string   `json:"testDir"`
	Write              bool     `json:"write"`
	Architecture       string   `json:"architecture"`
	GoVersion          string   `json:"goVersion"`
	Started            string   `json:"started"`
	Seconds            float64  `json:"seconds"`
	EstimatedDiskBytes int64    `json:"estimatedDiskBytes"`
	NativeWritable     bool     `json:"nativeWritable"`
	Checks             []string `json:"checks"`
	LiteralBytes       int64    `json:"literalBytes,omitempty"`
	ReusedBytes        int64    `json:"reusedBytes,omitempty"`
	DeltaBytes         int      `json:"deltaBytes,omitempty"`
	SignatureBytes     int      `json:"signatureBytes,omitempty"`
	Cleaned            bool     `json:"cleaned"`
}

func openParent(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "nas-sync-capability-test" {
		return nil, fmt.Errorf("test directory must be an existing canonical absolute nas-sync-capability-test child")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(path[1:], "/") {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

// Run defaults to metadata-only inspection. Write mode uses about 512 KiB of
// base data plus a one-byte insertion and small metadata; it never opens user
// files or enumerates the supplied parent. Cleanup is limited to its own unique
// child. It tests native reconstruction, not network transport or power loss.
func Run(ctx context.Context, path string, write bool) (report Report, err error) {
	start := time.Now()
	report = Report{Scope: "native disposable probe; not automatic-sync acceptance", TestDir: path, Write: write, Architecture: runtime.GOARCH, GoVersion: runtime.Version(), Started: start.UTC().Format(time.RFC3339Nano), EstimatedDiskBytes: EstimatedDiskBytes}
	defer func() { report.Seconds = time.Since(start).Seconds() }()
	parent, err := openParent(path)
	if err != nil {
		return report, err
	}
	defer parent.Close()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(parent.Fd()), &fs); err != nil {
		return report, err
	}
	switch uint64(uint32(fs.Type)) {
	case unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.SMB_SUPER_MAGIC, unix.NFS_SUPER_MAGIC, unix.FUSE_SUPER_MAGIC:
		return report, fmt.Errorf("run this probe on the NAS native filesystem, not a client network mount")
	}
	report.NativeWritable = fs.Flags&unix.ST_RDONLY == 0
	report.Checks = append(report.Checks, "canonical-existing-native-test-directory")
	if !write {
		return report, nil
	}
	if !report.NativeWritable {
		return report, fmt.Errorf("native test filesystem is read-only")
	}
	if fs.Bsize <= 0 || fs.Bavail < uint64((32*1024*1024)/fs.Bsize) {
		return report, fmt.Errorf("at least 32 MiB free native space required")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return report, err
	}
	name := "ananas-native-probe-" + hex.EncodeToString(random[:])
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return report, err
	}
	work := filepath.Join(path, name)
	defer func() {
		cleanup := os.RemoveAll(work)
		if cleanup == nil {
			cleanup = parent.Sync()
		}
		report.Cleaned = cleanup == nil
		err = errors.Join(err, cleanup)
	}()
	root, state := filepath.Join(work, "root"), filepath.Join(work, "state")
	if err := os.Mkdir(root, 0700); err != nil {
		return report, err
	}
	if err := os.Mkdir(state, 0700); err != nil {
		return report, err
	}
	namespace := hash.SumBytes([]byte(name)).Hex()
	coordinator, err := journal.Open(state, namespace)
	if err != nil {
		return report, err
	}
	defer func() {
		if coordinator != nil {
			err = errors.Join(err, coordinator.Close())
		}
	}()
	if competing, e := journal.Open(state, namespace); e == nil {
		competing.Close()
		return report, fmt.Errorf("competing native journal owner was not excluded")
	} else if !errors.Is(e, bolt.ErrTimeout) {
		return report, fmt.Errorf("second journal open failed without proving lock exclusion: %w", e)
	}
	report.Checks = append(report.Checks, "native-journal-lock-second-open-refused")
	store, err := stage.Open(state)
	if err != nil {
		return report, err
	}
	defer store.Close()
	publisher, err := publish.Open(root, state, coordinator.Version, nil, 2*1024*1024)
	if err != nil {
		return report, err
	}
	defer func() {
		if publisher != nil {
			err = errors.Join(err, publisher.Close())
		}
	}()
	report.Checks = append(report.Checks, "native-mount-identity-and-private-storage")
	baseBytes := make([]byte, FixtureBytes)
	if _, err := rand.Read(baseBytes); err != nil {
		return report, err
	}
	baseID := hash.SumBytes([]byte(name + "base")).Hex()
	nextID := hash.SumBytes([]byte(name + "next")).Hex()
	deleteID := hash.SumBytes([]byte(name + "delete")).Hex()
	directoryID := hash.SumBytes([]byte(name + "directory")).Hex()
	directoryDeleteID := hash.SumBytes([]byte(name + "directory-delete")).Hex()
	client := hash.SumBytes([]byte(name + "client")).Hex()
	const fixturePath = "folder/fixture.bin"
	directoryProposal := func(op, expected string, next journal.Version) journal.Proposal {
		return journal.Proposal{ID: hash.SumBytes([]byte(name + op)).Hex(), Client: client, Entries: []journal.Entry{{Path: "folder", Expected: expected, Next: next}}}
	}
	if _, err := coordinator.Commit(ctx, coordinator.Epoch(), directoryProposal("mkdir", "", journal.Version{ID: directoryID, Directory: true}), publisher); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "native-directory-create")
	empty, err := delta.Build(ctx, bytes.NewReader(nil), 0, 64*1024)
	if err != nil {
		return report, err
	}
	var initial bytes.Buffer
	if _, err := delta.Encode(ctx, bytes.NewReader(baseBytes), int64(len(baseBytes)), empty, &initial); err != nil {
		return report, err
	}
	if _, err := store.Receive(ctx, baseID, &initial, nil, 0, empty.Digest); err != nil {
		return report, err
	}
	base := journal.Version{ID: baseID, Size: int64(len(baseBytes)), Digest: hash.SumBytes(baseBytes)}
	proposal := func(op, expected string, next journal.Version) journal.Proposal {
		return journal.Proposal{ID: hash.SumBytes([]byte(name + op)).Hex(), Client: client, Entries: []journal.Entry{{Path: fixturePath, Expected: expected, Next: next}}}
	}
	if _, err := coordinator.Commit(ctx, coordinator.Epoch(), proposal("create", "", base), publisher); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "exclusive-create-file-and-directory-flush")
	baseFile, err := store.OpenReady(baseID)
	if err != nil {
		return report, err
	}
	defer baseFile.Close()
	sig, err := delta.Build(ctx, baseFile, base.Size, 64*1024)
	if err != nil {
		return report, err
	}
	sigBytes, err := sig.MarshalBinary()
	if err != nil {
		return report, err
	}
	report.SignatureBytes = len(sigBytes)
	nextBytes := append([]byte{99}, baseBytes...)
	var wire bytes.Buffer
	stats, err := delta.Encode(ctx, bytes.NewReader(nextBytes), int64(len(nextBytes)), sig, &wire)
	if err != nil {
		return report, err
	}
	report.LiteralBytes, report.ReusedBytes, report.DeltaBytes = stats.LiteralBytes, stats.ReusedBytes, wire.Len()
	if stats.LiteralBytes != 1 || stats.ReusedBytes != base.Size || wire.Len() > 1024 {
		return report, fmt.Errorf("unexpected delta reuse or stream budget")
	}
	if _, err := store.Receive(ctx, nextID, &wire, baseFile, base.Size, base.Digest); err != nil {
		return report, err
	}
	next := journal.Version{ID: nextID, Size: int64(len(nextBytes)), Digest: hash.SumBytes(nextBytes)}
	interrupted := errors.New("probe interruption after durable publication before journal commit")
	_, err = coordinator.Commit(ctx, coordinator.Epoch(), proposal("replace", baseID, next), afterPublish{publisher, interrupted})
	if !errors.Is(err, interrupted) {
		return report, fmt.Errorf("publication boundary failed: %w", err)
	}
	if _, pending, err := coordinator.Head(fixturePath); err != nil || !pending {
		return report, fmt.Errorf("prepared publication not retained: %w", err)
	}
	closeErr := publisher.Close()
	publisher = nil
	if closeErr != nil {
		return report, closeErr
	}
	oldEpoch := coordinator.Epoch()
	if err := coordinator.Close(); err != nil {
		return report, err
	}
	coordinator = nil
	coordinator, err = journal.Open(state, namespace)
	if err != nil {
		return report, err
	}
	publisher, err = publish.Open(root, state, coordinator.Version, nil, 2*1024*1024)
	if err != nil {
		return report, err
	}
	if coordinator.Epoch() <= oldEpoch {
		return report, fmt.Errorf("reopen did not advance epoch")
	}
	if recovered, err := coordinator.Recover(ctx, coordinator.Epoch(), publisher); err != nil || recovered == nil || !recovered.Committed {
		return report, fmt.Errorf("recover publication: %w", err)
	}
	if err := verify(filepath.Join(root, fixturePath), next); err != nil {
		return report, err
	}
	if err := verify(filepath.Join(state, nextID+".publish"), base); err != nil {
		return report, err
	}
	if err := verify(filepath.Join(state, baseID+".ready"), base); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "one-byte-shift-delta-native-reuse", "atomic-exchange-displaced-base-retained", "reopen-epoch-and-prepared-publication-recovery")
	tombstone := journal.Version{ID: deleteID, Tombstone: true}
	if _, err := coordinator.Commit(ctx, coordinator.Epoch(), proposal("delete", nextID, tombstone), publisher); err != nil {
		return report, err
	}
	if _, err := os.Lstat(filepath.Join(root, fixturePath)); !errors.Is(err, os.ErrNotExist) {
		return report, fmt.Errorf("visible deletion not verified")
	}
	if err := verify(filepath.Join(state, deleteID+".displaced"), next); err != nil {
		return report, err
	}
	if _, err := coordinator.Commit(ctx, coordinator.Epoch(), directoryProposal("rmdir", directoryID, journal.Version{ID: directoryDeleteID, Tombstone: true}), publisher); err != nil {
		return report, err
	}
	if _, err := os.Lstat(filepath.Join(root, "folder")); !errors.Is(err, os.ErrNotExist) {
		return report, fmt.Errorf("empty directory deletion not verified")
	}
	report.Checks = append(report.Checks, "empty-directory-delete")
	changes, err := coordinator.Changes(0, 6)
	if err != nil || len(changes) != 5 {
		return report, fmt.Errorf("unexpected committed journal length: %w", err)
	}
	for n := uint64(1); n <= 5; n++ {
		if err := coordinator.Acknowledge(client, n-1, n); err != nil {
			return report, err
		}
	}
	if cursor, err := coordinator.Cursor(client); err != nil || cursor != 5 {
		return report, fmt.Errorf("unexpected client cursor: %w", err)
	}
	report.Checks = append(report.Checks, "explicit-delete-retains-displaced-content", "contiguous-journal-and-client-cursor")
	return report, nil
}

type afterPublish struct {
	publisher journal.Publisher
	failure   error
}

func (a afterPublish) Publish(ctx context.Context, r journal.Record) error {
	if err := a.publisher.Publish(ctx, r); err != nil {
		return err
	}
	return a.failure
}
func verify(path string, want journal.Version) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != want.Size {
		return fmt.Errorf("probe file size mismatch")
	}
	d, err := hash.Sum(io.LimitReader(f, want.Size+1))
	if err != nil {
		return err
	}
	if d != want.Digest {
		return fmt.Errorf("probe content mismatch")
	}
	return nil
}
