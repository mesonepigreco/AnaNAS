package replica

import (
	"context"
	"errors"
	"fmt"
	"io"

	"nas-sync/internal/content"
	"nas-sync/internal/delta"
	"nas-sync/internal/hash"
	"nas-sync/internal/journal"
	"nas-sync/internal/stage"
)

// EncodeUpload reads an immutable local snapshot, encodes against an already
// authenticated remote base signature, and seals a bounded wire spool. No NAS
// request or fallback occurs here. The caller binds base to expected remote
// version/namespace, reserves storage, and persists outbox provenance before send.
func EncodeUpload(ctx context.Context, store *stage.Store, target journal.Version, base *delta.Signature, rate int64) (stage.WireInfo, delta.Stats, error) {
	var info stage.WireInfo
	var stats delta.Stats
	if store == nil || rate < 1024 || rate > 1<<30 || target.Directory || target.Tombstone || target.Size > 8<<30 {
		return info, stats, fmt.Errorf("bounded file snapshot and rate required")
	}
	if err := journal.ValidateVersion(target); err != nil {
		return info, stats, err
	}
	if err := base.Validate(); err != nil {
		return info, stats, err
	}
	if base.BlockSize != 64*1024 || base.Size > 8<<30 {
		return info, stats, fmt.Errorf("invalid upload base signature")
	}
	ctx, cancel := context.WithTimeout(ctx, contentTimeout(target.Size, rate))
	defer cancel()
	f, err := store.OpenReady(target.ID)
	if err != nil {
		return info, stats, err
	}
	defer f.Close()
	pace := newPacer(ctx, rate)
	maxBytes, err := delta.WireBound(target.Size)
	if err != nil {
		return info, stats, err
	}
	info, err = store.WriteWire(ctx, target.ID, maxBytes, func(out io.Writer) error {
		var err error
		stats, err = delta.Encode(ctx, &pacedReader{f, pace}, target.Size, base, &pacedWriter{out, pace})
		if err != nil {
			return err
		}
		if stats.Digest != target.Digest {
			return fmt.Errorf("upload snapshot content changed")
		}
		return nil
	})
	if err != nil {
		return stage.WireInfo{}, stats, err
	}
	return info, stats, nil
}

// OpenUploadWire verifies a recorded spool's size/digest before returning its
// read-only handle positioned at zero. Verification is bounded and paced; no
// wire object is trusted merely because its filename says ready.
func OpenUploadWire(ctx context.Context, store *stage.Store, id string, want stage.WireInfo, rate int64) (io.ReadCloser, error) {
	if store == nil || want.Size < delta.HeaderBytes+33 || want.Size > (8<<30)+int64(delta.MaxOperations)*45+delta.HeaderBytes+33 || rate < 1024 || rate > 1<<30 {
		return nil, fmt.Errorf("bounded wire verification required")
	}
	f, err := store.OpenWire(id)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || st.Size() != want.Size {
		f.Close()
		return nil, errors.Join(fmt.Errorf("wire spool size differs"), err)
	}
	verifyCtx, cancel := context.WithTimeout(ctx, contentTimeout(want.Size, rate))
	defer cancel()
	digest, err := hash.Sum(&pacedReader{io.LimitReader(f, want.Size+1), newPacer(verifyCtx, rate)})
	if err != nil || digest != want.Digest {
		f.Close()
		return nil, errors.Join(fmt.Errorf("wire spool digest differs"), err)
	}
	after, err := f.Stat()
	if err != nil || content.Fingerprint(st) != content.Fingerprint(after) {
		f.Close()
		return nil, errors.Join(fmt.Errorf("wire spool changed during verification"), err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	return &pacedReadCloser{Reader: &pacedReader{io.LimitReader(f, want.Size), newPacer(ctx, rate)}, Closer: f}, nil
}

type pacedReadCloser struct {
	io.Reader
	io.Closer
}
