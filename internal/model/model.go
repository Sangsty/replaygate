// Package model holds the types shared by the recorder, the API and the store.
package model

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Limits enforced when recording and ingesting requests.
const (
	// MaxBodyBytes is the largest request body the recorder captures. A request
	// with a bigger body is still proxied but is not recorded, because a
	// truncated body would not be a faithful replay.
	MaxBodyBytes = 64 << 10
	MaxPathLen   = 2048
	MaxQueryLen  = 4096
)

// RecordedRequest is one sanitized request observation sent by the recorder.
// The API computes the endpoint template and the fingerprint itself, so a
// client cannot choose how its requests are grouped or de-duplicated.
type RecordedRequest struct {
	Method    string              `json:"method"`
	Path      string              `json:"path"`
	Query     string              `json:"query"`
	Headers   map[string][]string `json:"headers"`
	Body      []byte              `json:"body,omitempty"`
	Status    int                 `json:"status"`
	LatencyMS int                 `json:"latency_ms"`
}

// IngestRequest is the body of POST /v1/projects/{pid}/ingest.
type IngestRequest struct {
	Requests []RecordedRequest `json:"requests"`
}

// IngestResponse is the reply to a successful ingest.
type IngestResponse struct {
	Accepted int `json:"accepted"`
}

// Validate reports why r cannot be stored, or nil. The MVP replays idempotent
// GET requests only, so anything else is rejected rather than silently kept.
func (r RecordedRequest) Validate() error {
	switch {
	case r.Method != http.MethodGet:
		return fmt.Errorf("method must be GET, got %q", r.Method)
	case !strings.HasPrefix(r.Path, "/"):
		return errors.New("path must start with /")
	case len(r.Path) > MaxPathLen:
		return fmt.Errorf("path longer than %d bytes", MaxPathLen)
	case len(r.Query) > MaxQueryLen:
		return fmt.Errorf("query longer than %d bytes", MaxQueryLen)
	case len(r.Body) > MaxBodyBytes:
		return fmt.Errorf("body larger than %d bytes", MaxBodyBytes)
	case r.Status < 100 || r.Status > 599:
		return fmt.Errorf("status %d out of range", r.Status)
	case r.LatencyMS < 0:
		return errors.New("latency_ms must not be negative")
	}
	return nil
}
