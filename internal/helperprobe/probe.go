// Package helperprobe performs a bounded explicit transport test against an
// authenticated helper that advertises its validated disposable-only scope.
package helperprobe

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"time"

	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/transferapi"
)

const FixtureBytes = 512 * 1024

type Report struct {
	Scope                   string              `json:"scope"`
	Write                   bool                `json:"write"`
	Seconds                 float64             `json:"seconds"`
	Epoch                   uint64              `json:"epoch"`
	Checks                  []string            `json:"checks"`
	UploadLiteral           int64               `json:"uploadLiteralBytes"`
	UploadReused            int64               `json:"uploadReusedBytes"`
	DownloadLiteral         int64               `json:"downloadLiteralBytes"`
	DownloadReused          int64               `json:"downloadReusedBytes"`
	UpdateDeltaBytes        int                 `json:"updateDeltaBytes"`
	Traffic                 transferapi.Traffic `json:"encryptedStreamTraffic"`
	NotificationIdleSeconds float64             `json:"notificationIdleSeconds,omitempty"`
	NotificationIdleTraffic transferapi.Traffic `json:"notificationIdleTraffic"`
}

func Run(ctx context.Context, c *transferapi.Client, clientID string, write bool) (report Report, err error) {
	if c == nil {
		return report, fmt.Errorf("authenticated client required")
	}
	start := time.Now()
	report.Scope = "explicit disposable helper transport probe; not autosync acceptance"
	report.Write = write
	defer func() { report.Seconds = time.Since(start).Seconds(); report.Traffic = c.Traffic() }()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	state, err := c.State(ctx)
	if err != nil {
		return report, err
	}
	report.Epoch = state.Epoch
	report.Checks = append(report.Checks, "authenticated-pinned-helper-state")
	watchCtx, stopWatch := context.WithCancel(ctx)
	hints, watchDone := make(chan struct{}, 1), make(chan struct{})
	var watchErr error
	go func() { watchErr = c.WatchChanges(watchCtx, hints); close(watchDone) }()
	defer func() { stopWatch(); <-watchDone }()
	awaitHint := func() error {
		select {
		case <-hints:
			return nil
		case <-watchDone:
			return fmt.Errorf("notification stream ended: %w", watchErr)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := awaitHint(); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "authenticated-initial-notification")
	if !write {
		// A bounded explicit test window, never an idle daemon polling loop.
		idleBefore, idleStart := c.Traffic(), time.Now()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case <-watchDone:
			return report, fmt.Errorf("quiet stream ended: %w", watchErr)
		case <-timer.C:
		}
		idleAfter := c.Traffic()
		report.NotificationIdleSeconds = time.Since(idleStart).Seconds()
		report.NotificationIdleTraffic = transferapi.Traffic{Sent: idleAfter.Sent - idleBefore.Sent, Received: idleAfter.Received - idleBefore.Received}
		if report.NotificationIdleTraffic != (transferapi.Traffic{}) {
			return report, fmt.Errorf("quiet notification stream generated encrypted traffic")
		}
		report.Checks = append(report.Checks, "quiet-notification-no-encrypted-traffic")
		return report, nil
	}
	if !state.Writes || state.Scope != "disposable-test" {
		return report, fmt.Errorf("authenticated disposable-test helper required before writes")
	}
	before, err := c.ChangesPage(ctx, "", "", 0, journal.MaxPage)
	if err != nil {
		return report, err
	}
	if before.Through != 0 || len(before.Batches) != 0 {
		return report, fmt.Errorf("fresh disposable namespace required")
	}
	report.Checks = append(report.Checks, "fresh-disposable-namespace")
	identity := func() (string, error) { id, err := hash.RandomDigest(); return id.Hex(), err }
	dirID, err := identity()
	if err != nil {
		return report, err
	}
	commit := func(path, expected string, next journal.Version, wire []byte) (transferapi.ApplyResponse, error) {
		id, err := identity()
		if err != nil {
			return transferapi.ApplyResponse{}, err
		}
		var reader io.Reader
		if wire != nil {
			reader = bytes.NewReader(wire)
		}
		return c.Apply(ctx, transferapi.ApplyRequest{Epoch: state.Epoch, Proposal: journal.Proposal{ID: id, Client: clientID, Entries: []journal.Entry{{Path: path, Expected: expected, Next: next}}}, DeltaBytes: []int64{int64(len(wire))}}, []io.Reader{reader})
	}
	if _, err := commit("probe", "", journal.Version{ID: dirID, Directory: true}, nil); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "native-directory-publication")
	if err := awaitHint(); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "authenticated-commit-notification")
	base := make([]byte, FixtureBytes)
	if _, err := rand.Read(base); err != nil {
		return report, err
	}
	empty, err := delta.Build(ctx, bytes.NewReader(nil), 0, 65536)
	if err != nil {
		return report, err
	}
	encode := func(data []byte, sig *delta.Signature) ([]byte, error) {
		var wire bytes.Buffer
		_, err := delta.Encode(ctx, bytes.NewReader(data), int64(len(data)), sig, &wire)
		return wire.Bytes(), err
	}
	baseWire, err := encode(base, empty)
	if err != nil {
		return report, err
	}
	baseID, err := identity()
	if err != nil {
		return report, err
	}
	baseVersion := journal.Version{ID: baseID, Size: int64(len(base)), Digest: hash.SumBytes(base)}
	if _, err := commit("probe/file", "", baseVersion, baseWire); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "native-initial-file-publication")
	signature, err := c.Signature(ctx, transferapi.Selection{Path: "probe/file", Version: baseID}, baseVersion)
	if err != nil {
		return report, err
	}
	target := append([]byte{17}, base...)
	wire, err := encode(target, signature)
	if err != nil {
		return report, err
	}
	report.UpdateDeltaBytes = len(wire)
	targetID, err := identity()
	if err != nil {
		return report, err
	}
	v := journal.Version{ID: targetID, Size: int64(len(target)), Digest: hash.SumBytes(target)}
	result, err := commit("probe/file", baseID, v, wire)
	if err != nil {
		return report, err
	}
	if len(result.Transfers) != 1 {
		return report, fmt.Errorf("missing upload transfer evidence")
	}
	report.UploadLiteral, report.UploadReused = result.Transfers[0].LiteralBytes, result.Transfers[0].ReusedBytes
	if report.UploadLiteral != 1 || report.UploadReused != FixtureBytes || report.UpdateDeltaBytes >= 4096 {
		return report, fmt.Errorf("native update did not reuse the expected base")
	}
	report.Checks = append(report.Checks, "one-byte-diff-upload-native-reuse")
	localSignature, err := delta.Build(ctx, bytes.NewReader(base), int64(len(base)), 65536)
	if err != nil {
		return report, err
	}
	var downloaded bytes.Buffer
	stats, err := c.DownloadVersion(ctx, "probe/file", v, localSignature, bytes.NewReader(base), &downloaded)
	if err != nil {
		return report, err
	}
	report.DownloadLiteral, report.DownloadReused = stats.LiteralBytes, stats.ReusedBytes
	if !bytes.Equal(downloaded.Bytes(), target) || stats.LiteralBytes != 1 || stats.ReusedBytes != FixtureBytes {
		return report, fmt.Errorf("download content or reuse differs")
	}
	report.Checks = append(report.Checks, "verified-one-byte-diff-download")
	deleteID, err := identity()
	if err != nil {
		return report, err
	}
	if _, err := commit("probe/file", targetID, journal.Version{ID: deleteID, Tombstone: true}, nil); err != nil {
		return report, err
	}
	dirDelete, err := identity()
	if err != nil {
		return report, err
	}
	if _, err := commit("probe", dirID, journal.Version{ID: dirDelete, Tombstone: true}, nil); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "explicit-file-and-empty-directory-delete")
	page, err := c.ChangesPage(ctx, before.Namespace, before.Policy, 0, journal.MaxPage)
	if err != nil {
		return report, err
	}
	if len(page.Batches) != 5 || page.Through != 5 {
		return report, fmt.Errorf("journal did not record exactly five commits")
	}
	head, err := c.Head(ctx, "probe/file")
	if err != nil {
		return report, err
	}
	if head.Pending || head.Version == nil || head.Version.ID != deleteID || !head.Version.Tombstone {
		return report, fmt.Errorf("final file tombstone differs")
	}
	report.Checks = append(report.Checks, "contiguous-journal-and-final-tombstone")
	return report, nil
}
