package transferapi

import (
	"context"
	"fmt"

	"nas-sync/internal/delta"
	"nas-sync/internal/journal"
)

func (c *Client) CompareHead(ctx context.Context, namespace, path string) (*journal.Version, bool, error) {
	if namespace != c.opts.Namespace {
		return nil, false, fmt.Errorf("comparison namespace differs from configured target")
	}
	head, err := c.Head(ctx, path)
	return head.Version, head.Pending, err
}

func (c *Client) CompareSignature(ctx context.Context, namespace, path string, want journal.Version) (*delta.Signature, error) {
	if namespace != c.opts.Namespace {
		return nil, fmt.Errorf("comparison namespace differs from configured target")
	}
	return c.Signature(ctx, Selection{Path: path, Version: want.ID}, want)
}
