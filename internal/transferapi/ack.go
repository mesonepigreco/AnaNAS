package transferapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"nas-sync/internal/changefeed"
	"nas-sync/internal/journal"
	"nas-sync/internal/manifest"
)

type acknowledgeRequest struct {
	Namespace  string   `json:"namespace"`
	Policy     string   `json:"policy"`
	Expected   uint64   `json:"expected"`
	Through    uint64   `json:"through"`
	Exclusions []string `json:"exclusions"`
}

type acknowledgeResponse struct {
	Namespace string `json:"namespace"`
	Policy    string `json:"policy"`
	Through   uint64 `json:"through"`
}

func (a acknowledgeRequest) validate() error {
	if !manifest.ValidID(a.Namespace) || !manifest.ValidID(a.Policy) || a.Through <= a.Expected || a.Through-a.Expected > journal.MaxPage {
		return fmt.Errorf("bounded acknowledgement identity/range required")
	}
	_, err := changefeed.Patterns(a.Exclusions)
	return err
}

func (s *Server) acknowledge(w http.ResponseWriter, r *http.Request, client string) {
	if !s.opts.Writes {
		fail(w, 403, fmt.Errorf("acknowledgement writes disabled"))
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, 400, fmt.Errorf("acknowledgement query not supported"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, changefeed.MaxRequestBytes+4)
	var a acknowledgeRequest
	if err := ReadFrame(r.Body, &a, changefeed.MaxRequestBytes); err != nil {
		fail(w, 400, err)
		return
	}
	if err := eof(r.Body); err != nil {
		fail(w, 400, err)
		return
	}
	if err := a.validate(); err != nil {
		fail(w, 400, err)
		return
	}
	if a.Namespace != s.c.Namespace() || a.Policy != changefeed.Policy(s.c.Namespace(), s.opts.Exclusions, a.Exclusions) {
		fail(w, 409, fmt.Errorf("acknowledgement namespace/policy differs"))
		return
	}
	if err := s.c.AcknowledgeRange(client, a.Policy, a.Expected, a.Through); err != nil {
		fail(w, 409, err)
		return
	}
	reply(w, 200, acknowledgeResponse{a.Namespace, a.Policy, a.Through})
}

// AcknowledgeChanges sends one bounded cumulative claim, only after the caller
// has durably completed these batches. It never fetches content, retries or polls.
// The TLS leaf allowlist supplies the server-side client identity, not this body.
func (c *Client) AcknowledgeChanges(ctx context.Context, namespace, policy string, expected, through uint64) error {
	if !c.opts.Writes {
		return fmt.Errorf("acknowledgement writes disabled")
	}
	a := acknowledgeRequest{namespace, policy, expected, through, c.opts.Exclusions}
	if err := a.validate(); err != nil {
		return err
	}
	if namespace != c.opts.Namespace {
		return fmt.Errorf("acknowledgement namespace differs")
	}
	ctx, finish, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	var body bytes.Buffer
	if err := WriteFrame(&body, a, changefeed.MaxRequestBytes); err != nil {
		return err
	}
	r, err := c.request(ctx, http.MethodPost, "/v1/ack", &body, int64(body.Len()))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	var response acknowledgeResponse
	if err := readJSON(r, &response, 1024); err != nil {
		return err
	}
	if response != (acknowledgeResponse{namespace, policy, through}) {
		return fmt.Errorf("acknowledgement response differs")
	}
	return nil
}
