// Package github is a small client for the two GitHub features replaygate
// uses: posting the report on a pull request and signing users in with OAuth.
// It uses only the standard library.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	defaultAPIURL = "https://api.github.com"
	maxPages      = 10 // 1000 comments; a PR with more than that will get a new report comment
	perPage       = 100
)

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Client posts pull request comments.
type Client struct {
	Token  string
	APIURL string       // default https://api.github.com
	HTTP   *http.Client // default: 15 second timeout
}

func (c *Client) base() string {
	if c.APIURL == "" {
		return defaultAPIURL
	}
	return strings.TrimRight(c.APIURL, "/")
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

type comment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// UpsertComment makes sure the pull request has exactly one comment containing
// marker, with the given body. It edits the existing one if it can and creates
// a new one otherwise. It returns the comment's URL.
//
// Needs a token with pull-requests: write (or issues: write). Pull requests
// from forks get a read-only token, so this fails there; callers should treat
// that as a warning, not as a failed check.
func (c *Client) UpsertComment(ctx context.Context, repo string, pr int, marker, body string) (string, error) {
	if !repoRe.MatchString(repo) {
		return "", fmt.Errorf("repository must look like owner/name, got %q", repo)
	}
	if pr <= 0 {
		return "", errors.New("pull request number must be positive")
	}
	if c.Token == "" {
		return "", errors.New("no GitHub token")
	}
	if marker == "" || !strings.Contains(body, marker) {
		return "", errors.New("body must contain the marker, or the next run cannot find this comment")
	}

	existing, err := c.findComment(ctx, repo, pr, marker)
	if err != nil {
		return "", err
	}
	if existing != nil {
		var out comment
		err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, existing.ID), map[string]string{"body": body}, &out)
		if err == nil {
			return out.HTMLURL, nil
		}
		var se *statusError
		if !errors.As(err, &se) || (se.Code != http.StatusForbidden && se.Code != http.StatusNotFound) {
			return "", err
		}
		// Someone else's comment carries our marker, or it was deleted
		// meanwhile. Fall through and post our own.
	}
	var out comment
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), map[string]string{"body": body}, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

func (c *Client) findComment(ctx context.Context, repo string, pr int, marker string) (*comment, error) {
	for page := 1; page <= maxPages; page++ {
		var batch []comment
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&page=%d", repo, pr, perPage, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		for i := range batch {
			if strings.Contains(batch[i].Body, marker) {
				return &batch[i], nil
			}
		}
		if len(batch) < perPage {
			break
		}
	}
	return nil, nil
}

// statusError is a non-2xx answer from GitHub.
type statusError struct {
	Code int
	Msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("GitHub API %d: %s", e.Code, e.Msg) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "replaygate")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("call GitHub: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &statusError{Code: resp.StatusCode, Msg: errorMessage(raw)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode GitHub response: %w", err)
		}
	}
	return nil
}

// errorMessage extracts GitHub's "message" field, or a clipped copy of the body.
func errorMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
