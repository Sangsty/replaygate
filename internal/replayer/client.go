package replayer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrLeaseLost means the API reassigned the job; the worker must drop its results.
var ErrLeaseLost = errors.New("job lease lost")

// ErrPermanent marks errors that retrying cannot fix (bad key, wrong project).
var ErrPermanent = errors.New("permanent API error")

// Job is a leased batch of requests.
type Job struct {
	JobID    int64     `json:"job_id"`
	RunID    string    `json:"run_id"`
	Variant  string    `json:"variant"`
	Requests []Request `json:"requests"`
}

// Client talks to the replaygate API as one project.
type Client struct {
	base      string
	projectID string
	apiKey    string
	http      *http.Client
}

// NewClient returns a client for the API at baseURL.
func NewClient(baseURL, projectID, apiKey string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"), projectID: projectID, apiKey: apiKey,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, path string, in, out any) (int, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/projects/"+c.projectID+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("call API: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusConflict && strings.Contains(string(raw), "lease lost"):
		return resp.StatusCode, ErrLeaseLost
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests:
		return resp.StatusCode, fmt.Errorf("%w: %s: %s", ErrPermanent, resp.Status, strings.TrimSpace(string(raw)))
	case resp.StatusCode >= 400:
		return resp.StatusCode, fmt.Errorf("API %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode API response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Claim leases the next job of a variant. It returns (nil, nil) when there is none.
func (c *Client) Claim(ctx context.Context, workerID, variant string, lease time.Duration) (*Job, error) {
	var job Job
	status, err := c.do(ctx, "/jobs/claim", map[string]any{
		"worker_id": workerID, "variant": variant, "lease_seconds": int(lease.Seconds()),
	}, &job)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &job, nil
}

// Complete uploads a batch of results.
func (c *Client) Complete(ctx context.Context, workerID string, jobID int64, results []Result) error {
	_, err := c.do(ctx, fmt.Sprintf("/jobs/%d/complete", jobID), map[string]any{
		"worker_id": workerID, "results": results,
	}, nil)
	return err
}

// Fail reports that the worker could not process a job.
func (c *Client) Fail(ctx context.Context, workerID string, jobID int64, msg string) error {
	_, err := c.do(ctx, fmt.Sprintf("/jobs/%d/fail", jobID), map[string]any{
		"worker_id": workerID, "error": msg,
	}, nil)
	return err
}
