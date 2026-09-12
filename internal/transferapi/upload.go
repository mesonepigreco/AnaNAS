package transferapi

import (
	"context"
	"fmt"
	"io"

	"nas-sync/internal/journal"
)

// These methods implement the upload dispatch contract while keeping transport
// framing and the immutable configured namespace inside the client.
func (c *Client) UploadState(ctx context.Context, namespace string) (uint64, bool, error) {
	if namespace != c.opts.Namespace {
		return 0, false, fmt.Errorf("upload namespace differs from configured target")
	}
	state, err := c.State(ctx)
	return state.Epoch, state.Writes, err
}

func (c *Client) CommitUpload(ctx context.Context, namespace string, epoch uint64, proposal journal.Proposal, lengths []int64, streams []io.Reader) (journal.Record, error) {
	if namespace != c.opts.Namespace {
		return journal.Record{}, fmt.Errorf("upload namespace differs from configured target")
	}
	result, err := c.Apply(ctx, ApplyRequest{Epoch: epoch, Proposal: proposal, DeltaBytes: lengths}, streams)
	return result.Record, err
}

func (c *Client) RecoverUpload(ctx context.Context, namespace string, epoch uint64, proposal journal.Proposal) (*journal.Record, error) {
	if namespace != c.opts.Namespace {
		return nil, fmt.Errorf("upload namespace differs from configured target")
	}
	return c.Recover(ctx, RecoverRequest{Epoch: epoch, Operation: proposal.ID}, proposal)
}
