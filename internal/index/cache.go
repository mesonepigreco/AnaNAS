package index

import (
	"context"
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"nas-sync/internal/changefeed"
	"nas-sync/internal/journal"
)

// WithCachePins freezes replica references for one bounded maintenance batch.
// The journal owner must be acquired BEFORE this callback (the same lock order
// as publication). Do not call index methods from action. Observation and UI
// writes wait until action returns; no filesystem in either sync root is read.
func (d *DB) WithCachePins(ctx context.Context, candidates []journal.Entry, action func(map[string]bool) error) error {
	if action == nil {
		return fmt.Errorf("cache action required")
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		pins := map[string]bool{}
		pin := func(id string) {
			if id != "" {
				pins[id] = true
			}
		}
		if u, err := readUpload(tx); err != nil {
			return err
		} else if u != nil {
			for _, e := range u.Proposal.Entries {
				pin(e.Expected)
				pin(e.Next.ID)
			}
		}
		if p, err := readUploadPreparation(tx); err != nil {
			return err
		} else if p != nil {
			for _, s := range p.Sources {
				pin(s.Expected)
				pin(s.ID)
			}
		}
		for _, e := range candidates {
			if err := ctx.Err(); err != nil {
				return err
			}
			m, err := readBase(tx, e.Path)
			if err != nil {
				return err
			}
			if m != nil {
				pin(m.Content.ID)
			}
			s, err := readPathState(tx, e.Path)
			if err != nil {
				return err
			}
			if s.Conflict != nil {
				pin(s.Conflict.BaseID)
				pin(s.Conflict.LocalID)
				pin(s.Conflict.RemoteID)
			}
			issue, err := readIncoming(tx.Bucket(incomingIssues), e.Path)
			if err != nil {
				return err
			}
			if issue != nil {
				pin(issue.Expected)
				pin(issue.Remote.ID)
				pin(issue.Revision)
			}
		}
		if b := tx.Bucket(remoteInbox); b != nil {
			if err := b.ForEach(func(k, v []byte) error {
				var batch changefeed.Batch
				if err := json.Unmarshal(v, &batch); err != nil {
					return err
				}
				for _, e := range batch.Entries {
					pin(e.Expected)
					pin(e.Next.ID)
				}
				return ctx.Err()
			}); err != nil {
				return err
			}
		}
		return action(pins)
	})
}
