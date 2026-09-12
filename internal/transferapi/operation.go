package transferapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

type OperationResponse struct {
	Namespace string          `json:"namespace"`
	Record    *journal.Record `json:"record"`
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request, client string) {
	id := r.URL.Query().Get("id")
	if !manifest.ValidID(id) {
		fail(w, 400, fmt.Errorf("invalid operation ID"))
		return
	}
	record, err := s.c.Operation(id)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if record != nil {
		if record.Client != client {
			fail(w, 403, fmt.Errorf("operation belongs to another client"))
			return
		}
		for _, e := range record.Entries {
			if err := s.allowedPath(e.Path, e.Next.Directory || e.Next.Tombstone); err != nil {
				fail(w, 403, err)
				return
			}
		}
	}
	reply(w, 200, OperationResponse{Namespace: s.c.Namespace(), Record: record})
}

// Operation reads the durable outcome for this authenticated client's exact ID.
// A nil record is not an acknowledgement or automatic permission to retry.
func (c *Client) Operation(ctx context.Context, namespace, id string) (*journal.Record, error) {
	if !manifest.ValidID(namespace) || !manifest.ValidID(id) {
		return nil, fmt.Errorf("namespace and operation ID required")
	}
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	r, err := c.request(ctx, http.MethodGet, "/v1/operation?id="+url.QueryEscape(id), nil, 0)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var result OperationResponse
	if err := readJSON(r, &result, journal.MaxRecordBytes+1024); err != nil {
		return nil, err
	}
	if result.Namespace != namespace {
		return nil, fmt.Errorf("operation namespace differs")
	}
	record := result.Record
	if record != nil {
		if record.ID != id || record.Client != c.opts.ClientID || record.Epoch == 0 || record.Sequence == 0 {
			return nil, fmt.Errorf("operation identity or ordering differs")
		}
		if err := journal.ValidateProposal(record.Proposal); err != nil {
			return nil, err
		}
		for _, e := range record.Entries {
			if err := c.path(e.Path, e.Next.Directory || e.Next.Tombstone); err != nil {
				return nil, err
			}
			if err := c.version(e.Next); err != nil {
				return nil, err
			}
		}
	}
	return record, nil
}
