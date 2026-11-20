package gate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// Context is what the CI environment says about the change being checked.
type Context struct {
	CommitSHA string
	PRNumber  *int
}

// GitHubContext works out the commit and pull request number from the
// variables GitHub Actions sets. Explicit values (RG_COMMIT_SHA, RG_PR_NUMBER)
// win, then the pull_request event payload, then GITHUB_SHA.
//
// On pull_request events GITHUB_SHA is a synthetic merge commit that exists
// nowhere in the repository, so the event's head SHA, which is the commit the
// author pushed, is what gets recorded.
func GitHubContext(getenv func(string) string, readFile func(string) ([]byte, error)) (Context, error) {
	var c Context
	var eventSHA string
	if path := getenv("GITHUB_EVENT_PATH"); path != "" {
		if raw, err := readFile(path); err == nil {
			var ev struct {
				PullRequest struct {
					Number int `json:"number"`
					Head   struct {
						SHA string `json:"sha"`
					} `json:"head"`
				} `json:"pull_request"`
			}
			if json.Unmarshal(raw, &ev) == nil && ev.PullRequest.Number > 0 {
				n := ev.PullRequest.Number
				c.PRNumber = &n
				eventSHA = ev.PullRequest.Head.SHA
			}
		}
	}

	if v := getenv("RG_PR_NUMBER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Context{}, fmt.Errorf("RG_PR_NUMBER must be a positive integer, got %q", v)
		}
		c.PRNumber = &n
	}
	for _, candidate := range []string{getenv("RG_COMMIT_SHA"), eventSHA, getenv("GITHUB_SHA")} {
		if candidate != "" {
			c.CommitSHA = candidate
			break
		}
	}
	if !shaRe.MatchString(c.CommitSHA) {
		return Context{}, fmt.Errorf("cannot determine the commit: set RG_COMMIT_SHA to a 7 to 64 character hex SHA (got %q)", c.CommitSHA)
	}
	return c, nil
}
