package replica

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"nas-sync/internal/index"
	"nas-sync/internal/journal"
)

type OperationReader interface {
	Operation(context.Context, string, string) (*journal.Record, error)
}

type UploadRecovery struct {
	Operation string
	// idle: no outbox; unrecorded: remote has no journal record; prepared:
	// remote publication needs recovery; committed: receipt retained locally.
	State string
}

// RecoverUploadReceipt performs at most one scoped metadata request and saves
// only the exact authenticated result. It never sends payload, retries an
// upload, abandons staging, clears dirty work or grants publication permission.
// An unrecorded result may still have orphan remote staging; it is not proof
// that retry/cleanup is safe. The caller owns scheduling and policy changes.
func RecoverUploadReceipt(ctx context.Context, db *index.DB, remote OperationReader, gate func(context.Context) error) (UploadRecovery, error) {
	var result UploadRecovery
	if db == nil || remote == nil || gate == nil {
		return result, fmt.Errorf("outbox, remote and policy gate required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	u, err := db.PendingUpload()
	if err != nil {
		return result, err
	}
	if u == nil {
		result.State = "idle"
		return result, nil
	}
	result.Operation = u.Proposal.ID
	if u.Receipt != nil {
		result.State = "committed"
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := gate(ctx); err != nil {
		return result, err
	}
	record, err := remote.Operation(ctx, u.Namespace, u.Proposal.ID)
	if err != nil {
		return result, err
	}
	if record == nil {
		result.State = "unrecorded"
		return result, nil
	}
	if !reflect.DeepEqual(record.Proposal, u.Proposal) {
		return result, journal.ErrIdentity
	}
	if record.Sequence == 0 || record.Epoch == 0 {
		return result, fmt.Errorf("invalid remote operation ordering")
	}
	if !record.Committed {
		result.State = "prepared"
		return result, nil
	}
	if err := db.RecordUploadCommit(u.Namespace, *record); err != nil {
		return result, err
	}
	result.State = "committed"
	return result, nil
}
