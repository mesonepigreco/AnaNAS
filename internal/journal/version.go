package journal

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/hash"
	"nas-sync/internal/manifest"
)

func validatePath(path string) error {
	if len(path) > 4096 || path == "." || !fs.ValidPath(path) || strings.ContainsAny(path, "\\\x00") {
		return fmt.Errorf("invalid proposal path")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".nas-sync" {
			return fmt.Errorf("internal path excluded")
		}
	}
	return nil
}

// ValidateVersion checks one immutable description without constructing a
// proposal or allocating a whole manifest. It performs no content verification.
func ValidateVersion(v Version) error {
	if !manifest.ValidID(v.ID) || v.Size < 0 || v.Size > int64(hash.MaxBlockSize)*manifest.MaxBlocks {
		return fmt.Errorf("invalid version identity or size")
	}
	if v.Tombstone && v.Directory {
		return fmt.Errorf("version cannot be both directory and tombstone")
	}
	if (v.Tombstone || v.Directory) && (v.Size != 0 || v.Digest != (hash.Digest{})) {
		return fmt.Errorf("directory or tombstone contains file content")
	}
	if !v.Tombstone && !v.Directory && v.Size == 0 && v.Digest != hash.SumBytes(nil) {
		return fmt.Errorf("invalid empty-file digest")
	}
	return nil
}

// Path is stored atomically with the immutable version and journal commit. It
// permits exact historical lookup without listing heads or scanning history.
type storedVersion struct {
	Version
	Path string `json:"path,omitempty"`
}

func readVersion(tx *bolt.Tx, id string) (storedVersion, error) {
	var v storedVersion
	raw := tx.Bucket(versionsBucket).Get([]byte(id))
	// A 4096-byte path may expand sixfold when JSON-escaped.
	if len(raw) == 0 || len(raw) > 32*1024 {
		return v, fmt.Errorf("missing or oversized version metadata")
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	if v.ID != id {
		return v, ErrIdentity
	}
	if err := ValidateVersion(v.Version); err != nil {
		return v, err
	}
	if v.Path != "" {
		if err := validatePath(v.Path); err != nil {
			return v, err
		}
	}
	return v, nil
}

// VersionAt returns a committed version bound to the exact supplied path, even
// if it is no longer that path's head. It never opens content or scans history.
// Legacy metadata without a path binding is accepted only for its current head;
// absent historical binding is an error, not permission to use another path.
func (c *Coordinator) VersionAt(path, id string) (*Version, bool, error) {
	if err := validatePath(path); err != nil {
		return nil, false, err
	}
	if !manifest.ValidID(id) {
		return nil, false, fmt.Errorf("invalid version ID")
	}
	var version *Version
	var pending bool
	err := c.db.View(func(tx *bolt.Tx) error {
		pending = tx.Bucket(metaBucket).Get([]byte("pending")) != nil
		v, err := readVersion(tx, id)
		if err != nil {
			return err
		}
		if v.Path != path {
			if v.Path != "" || string(tx.Bucket(headsBucket).Get([]byte(path))) != id {
				return ErrIdentity
			}
		}
		version = &v.Version
		return nil
	})
	return version, pending, err
}

// CheckUnusedVersions is a bounded ownership check for local upload preparation
// and abort. The caller serializes all replica operations while using its result;
// this read alone is not a reservation against another goroutine's publication.
func (c *Coordinator) CheckUnusedVersions(ids []string) error {
	if len(ids) < 1 || len(ids) > MaxEntries {
		return fmt.Errorf("bounded candidate IDs required")
	}
	for _, id := range ids {
		if !manifest.ValidID(id) {
			return fmt.Errorf("invalid candidate ID")
		}
	}
	return c.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(metaBucket).Get([]byte("pending")) != nil || tx.Bucket(metaBucket).Get(intakeKey) != nil {
			return ErrPending
		}
		for _, id := range ids {
			if tx.Bucket(versionsBucket).Get([]byte(id)) != nil {
				return ErrIdentity
			}
		}
		return nil
	})
}
