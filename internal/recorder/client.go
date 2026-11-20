package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Sangsty/replaygate/internal/model"
	"github.com/Sangsty/replaygate/internal/sanitize"
)

// Client talks to the replaygate API on behalf of one project.
type Client struct {
	baseURL   string
	projectID string
	apiKey    string
	http      *http.Client
}

// NewClient returns a Client for the API at baseURL, authenticated with the
// project's API key.
func NewClient(baseURL, projectID, apiKey string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		projectID: projectID,
		apiKey:    apiKey,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) projectURL(suffix string) string {
	return c.baseURL + "/v1/projects/" + c.projectID + suffix
}

// Upload sends one batch to the ingest endpoint. Client errors that retrying
// cannot fix (bad key, rejected payload) are wrapped with ErrPermanent;
// timeouts, 429 and 5xx are returned as plain errors so the caller retries.
func (c *Client) Upload(ctx context.Context, reqs []model.RecordedRequest) error {
	payload, err := json.Marshal(model.IngestRequest{Requests: reqs})
	if err != nil {
		return fmt.Errorf("%w: encode batch: %v", ErrPermanent, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.projectURL("/ingest"), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: build request: %v", ErrPermanent, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()
	return c.checkStatus(resp, "upload")
}

// Rules fetches the project's sanitizer rules.
func (c *Client) Rules(ctx context.Context) (sanitize.Rules, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.projectURL("/config"), nil)
	if err != nil {
		return sanitize.Rules{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return sanitize.Rules{}, fmt.Errorf("fetch rules: %w", err)
	}
	defer resp.Body.Close()
	if err := c.checkStatus(resp, "fetch rules"); err != nil {
		return sanitize.Rules{}, err
	}
	var body struct {
		SanitizeRules sanitize.Rules `json:"sanitize_rules"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return sanitize.Rules{}, fmt.Errorf("fetch rules: decode response: %w", err)
	}
	return body.SanitizeRules, nil
}

func (c *Client) checkStatus(resp *http.Response, what string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err := fmt.Errorf("%s: API returned %d: %s", what, resp.StatusCode, strings.TrimSpace(string(snippet)))
	retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout
	if !retryable {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return err
}
