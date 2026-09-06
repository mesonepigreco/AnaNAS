// Package diff contains pure, bounded decisions for the future transfer engine.
// It never reads files, contacts a NAS, or chooses a winner for a conflict.
package diff

import (
	"fmt"

	"nas-sync/internal/hash"
)

// Action describes the safe next step for one path after comparing it with the
// last acknowledged base.
type Action uint8

const (
	Noop Action = iota
	Push
	Pull
	Conflict
)

func (a Action) String() string {
	switch a {
	case Noop:
		return "noop"
	case Push:
		return "push"
	case Pull:
		return "pull"
	case Conflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// Block is a content-addressed fixed-size block in a manifest. Size is kept
// beside the digest so a transfer scheduler can account for bytes before I/O.
type Block struct {
	Digest hash.Digest
	Size   int64
}

// Manifest is the immutable content description used by the planner. A
// tombstone has no blocks and represents an intentional deletion. ID is an
// opaque version identity; it is never ordered by wall-clock time.
type Manifest struct {
	ID        string
	Size      int64
	Blocks    []Block
	Tombstone bool
}

// Decision is the pure result of comparing a local and remote head with their
// acknowledged base. Reason is diagnostic text for the UI and logs.
type Decision struct {
	Action Action
	Reason string
}

// Plan classifies a path conservatively. Equal content converges without a
// transfer even when independent versions have different IDs. Otherwise only
// one side being exactly at the acknowledged base permits a push or pull; two
// changed sides always produce a conflict.
func Plan(base, local, remote *Manifest) Decision {
	if sameContent(local, remote) {
		return Decision{Action: Noop, Reason: "local and remote content agree"}
	}
	localAtBase := sameHead(local, base)
	remoteAtBase := sameHead(remote, base)
	switch {
	case localAtBase && remoteAtBase:
		return Decision{Action: Noop, Reason: "both sides match the acknowledged base"}
	case remoteAtBase:
		return Decision{Action: Push, Reason: "only the local side diverged from the base"}
	case localAtBase:
		return Decision{Action: Pull, Reason: "only the remote side diverged from the base"}
	default:
		return Decision{Action: Conflict, Reason: "both sides diverged from the acknowledged base"}
	}
}

// MissingBlocks returns each local block whose digest is not already available
// remotely, once and in manifest order. It is intentionally a pure helper: the
// caller must obtain the availability set from a verified transport operation.
func MissingBlocks(local *Manifest, remoteHave map[hash.Digest]struct{}) []Block {
	if local == nil || local.Tombstone {
		return nil
	}
	missing := make([]Block, 0, len(local.Blocks))
	seen := make(map[hash.Digest]struct{}, len(local.Blocks))
	for _, block := range local.Blocks {
		if _, ok := remoteHave[block.Digest]; ok {
			continue
		}
		if _, ok := seen[block.Digest]; ok {
			continue
		}
		seen[block.Digest] = struct{}{}
		missing = append(missing, block)
	}
	return missing
}

// Validate checks bounded manifest structure before a caller allocates based on
// it. Block-size validation belongs to the configured transfer engine; this
// function only rejects malformed sizes and excessive block counts.
func (m *Manifest) Validate(maxBlocks int) error {
	if m == nil {
		return nil
	}
	if m.Size < 0 {
		return fmt.Errorf("manifest size cannot be negative")
	}
	if maxBlocks < 1 || maxBlocks > 1<<30 {
		return fmt.Errorf("max blocks out of range")
	}
	if len(m.Blocks) > maxBlocks {
		return fmt.Errorf("manifest has %d blocks; maximum is %d", len(m.Blocks), maxBlocks)
	}
	if m.Tombstone && (m.Size != 0 || len(m.Blocks) != 0) {
		return fmt.Errorf("tombstone cannot contain content")
	}
	var total int64
	for i, block := range m.Blocks {
		if block.Size <= 0 {
			return fmt.Errorf("block %d has invalid size %d", i, block.Size)
		}
		if block.Size > (1<<63-1)-total {
			return fmt.Errorf("block sizes overflow manifest size")
		}
		total += block.Size
	}
	if !m.Tombstone && total != m.Size {
		return fmt.Errorf("manifest size %d does not match blocks total %d", m.Size, total)
	}
	return nil
}

func sameHead(a, b *Manifest) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.ID != "" || b.ID != "" {
		return a.ID != "" && a.ID == b.ID
	}
	return sameContent(a, b)
}

func sameContent(a, b *Manifest) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Tombstone || b.Tombstone {
		return a.Tombstone && b.Tombstone
	}
	if a.Size != b.Size || len(a.Blocks) != len(b.Blocks) {
		return false
	}
	for i := range a.Blocks {
		if a.Blocks[i] != b.Blocks[i] {
			return false
		}
	}
	return true
}
