package transferapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/journal"
)

func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, changefeed.MaxRequestBytes+4)
	var request changefeed.Request
	if err := ReadFrame(r.Body, &request, changefeed.MaxRequestBytes); err != nil {
		fail(w, 400, err)
		return
	}
	if err := eof(r.Body); err != nil {
		fail(w, 400, err)
		return
	}
	if err := request.Validate(); err != nil {
		fail(w, 400, err)
		return
	}
	// Reject changed policy before reading journal records.
	policy := changefeed.Policy(s.c.Namespace(), s.opts.Exclusions, request.Exclusions)
	if request.Namespace != "" && (request.Namespace != s.c.Namespace() || request.Policy != policy) {
		fail(w, 409, fmt.Errorf("feed namespace or exclusion policy changed"))
		return
	}
	records, err := s.c.Changes(request.After, request.Limit)
	if err != nil {
		fail(w, 409, err)
		return
	}
	page, err := changefeed.Filter(s.c.Namespace(), s.opts.Exclusions, request, records)
	if err != nil {
		fail(w, 409, err)
		return
	}
	reply(w, 200, page)
}

// Changes reads one bounded page, with no polling or acknowledgement. Empty
// namespace/policy is allowed only when bootstrapping from sequence zero.
// Callers must durably retain every batch before advancing their receive cursor.
func (c *Client) ChangesPage(ctx context.Context, namespace, policy string, after uint64, limit int) (changefeed.Page, error) {
	var page changefeed.Page
	request := changefeed.Request{Namespace: namespace, Policy: policy, After: after, Limit: limit, Exclusions: c.opts.Exclusions}
	if err := request.Validate(); err != nil {
		return page, err
	}
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return page, err
	}
	defer finish()
	var frame bytes.Buffer
	if err := WriteFrame(&frame, request, changefeed.MaxRequestBytes); err != nil {
		return page, err
	}
	r, err := c.request(ctx, http.MethodPost, "/v1/changes", &frame, int64(frame.Len()))
	if err != nil {
		return page, err
	}
	defer r.Body.Close()
	if err := readJSON(r, &page, int64(limit*journal.MaxRecordBytes+4096)); err != nil {
		return changefeed.Page{}, err
	}
	if err := page.Validate(after, limit); err != nil {
		return changefeed.Page{}, err
	}
	if namespace != "" && (page.Namespace != namespace || page.Policy != policy) {
		return changefeed.Page{}, fmt.Errorf("feed identity changed")
	}
	for _, batch := range page.Batches {
		for _, entry := range batch.Entries {
			if err := c.path(entry.Path, entry.Next.Directory || entry.Next.Tombstone); err != nil {
				return changefeed.Page{}, err
			}
		}
	}
	return page, nil
}
