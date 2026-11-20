package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// loginRe is GitHub's rule for usernames: alphanumerics and single hyphens,
// at most 39 characters. (Consecutive hyphens are rejected by GitHub itself;
// the database check is deliberately a little looser.)
var loginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// ProjectSummary is a project as listed in the dashboard.
type ProjectSummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// AddProjectMember lets a GitHub user open the project in the dashboard.
// Adding the same login twice is not an error.
func (s *Store) AddProjectMember(ctx context.Context, projectID, login string) error {
	if !loginRe.MatchString(login) {
		return fmt.Errorf("%w: %q is not a valid GitHub login", ErrInvalid, login)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO project_members (project_id, github_login) VALUES ($1::uuid, $2)
		ON CONFLICT DO NOTHING`, projectID, strings.ToLower(login))
	if err != nil {
		return fmt.Errorf("add project member: %w", err)
	}
	return nil
}

// ProjectsForUser lists the projects a signed-in user can open: the ones they
// own and the ones they were added to.
func (s *Store) ProjectsForUser(ctx context.Context, userID, login string) ([]ProjectSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id::text, p.name, p.created_at
		FROM projects p
		WHERE p.owner_id = $1::uuid
		   OR EXISTS (SELECT 1 FROM project_members m WHERE m.project_id = p.id AND m.github_login = lower($2))
		ORDER BY p.name, p.id`, userID, login)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	out := []ProjectSummary{}
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CanAccessProject reports whether the user owns the project or was added to it.
func (s *Store) CanAccessProject(ctx context.Context, projectID, userID, login string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM projects p WHERE p.id = $1::uuid AND (
		    p.owner_id = $2::uuid
		    OR EXISTS (SELECT 1 FROM project_members m WHERE m.project_id = p.id AND m.github_login = lower($3))))`,
		projectID, userID, login).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check project access: %w", err)
	}
	return ok, nil
}

// ListRuns returns the project's most recent runs, newest first.
func (s *Store) ListRuns(ctx context.Context, projectID string, limit int) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, commit_sha, pr_number, status, total_requests,
		       breaking_count, additive_count, latency_count, error, created_at, finished_at
		FROM replay_runs WHERE project_id = $1::uuid
		ORDER BY created_at DESC, id LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		var errMsg *string
		if err := rows.Scan(&r.ID, &r.CommitSHA, &r.PRNumber, &r.Status, &r.TotalRequests,
			&r.Breaking, &r.Additive, &r.Latency, &errMsg, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		if errMsg != nil {
			r.Error = *errMsg
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NoiseField is a field a run learned to ignore.
type NoiseField struct {
	Endpoint string `json:"endpoint"`
	JSONPath string `json:"json_path"`
}

// RunNoise lists the fields the run learned to ignore.
func (s *Store) RunNoise(ctx context.Context, runID string) ([]NoiseField, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT endpoint, json_path FROM noise_fields WHERE run_id = $1::uuid ORDER BY endpoint, json_path`, runID)
	if err != nil {
		return nil, fmt.Errorf("load noise: %w", err)
	}
	defer rows.Close()
	out := []NoiseField{}
	for rows.Next() {
		var n NoiseField
		if err := rows.Scan(&n.Endpoint, &n.JSONPath); err != nil {
			return nil, fmt.Errorf("scan noise: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// RequestInfo identifies a recorded request.
type RequestInfo struct {
	ID       int64  `json:"id"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Query    string `json:"query"`
	Endpoint string `json:"endpoint"`
}

// ResponseDetail is one replayed answer, as stored (already redacted).
type ResponseDetail struct {
	Status    int    `json:"status"`
	Body      string `json:"body"`
	Truncated bool   `json:"truncated"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// RequestDetail is everything the side-by-side viewer needs for one request.
type RequestDetail struct {
	Request   RequestInfo     `json:"request"`
	Baseline  *ResponseDetail `json:"baseline"`  // nil if the baseline never answered
	Candidate *ResponseDetail `json:"candidate"` // nil if the candidate never answered
	Diffs     []DiffRow       `json:"diffs"`
}

// RunRequestDetail loads the baseline and candidate responses for one request
// of a run, plus that request's findings. It returns ErrNotFound unless the
// run belongs to the project and the request was part of it.
func (s *Store) RunRequestDetail(ctx context.Context, projectID, runID string, requestID int64) (RequestDetail, error) {
	var d RequestDetail
	err := s.pool.QueryRow(ctx, `
		SELECT rr.id, rr.method, rr.path, rr.query, rr.endpoint
		FROM replay_runs r
		JOIN recorded_requests rr ON rr.project_id = r.project_id
		WHERE r.id = $1::uuid AND r.project_id = $2::uuid AND rr.id = $3
		  AND EXISTS (SELECT 1 FROM replay_responses x WHERE x.run_id = r.id AND x.request_id = rr.id)`,
		runID, projectID, requestID).
		Scan(&d.Request.ID, &d.Request.Method, &d.Request.Path, &d.Request.Query, &d.Request.Endpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return RequestDetail{}, ErrNotFound
	}
	if err != nil {
		return RequestDetail{}, fmt.Errorf("load request: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT variant, COALESCE(status, 0), COALESCE(convert_from(body, 'UTF8'), ''), truncated,
		       COALESCE(latency_ms, 0), COALESCE(error, '')
		FROM replay_responses
		WHERE run_id = $1::uuid AND request_id = $2 AND variant IN ('baseline_a', 'candidate')`, runID, requestID)
	if err != nil {
		return RequestDetail{}, fmt.Errorf("load responses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var variant string
		var r ResponseDetail
		if err := rows.Scan(&variant, &r.Status, &r.Body, &r.Truncated, &r.LatencyMS, &r.Error); err != nil {
			return RequestDetail{}, fmt.Errorf("scan response: %w", err)
		}
		if variant == VariantCandidate {
			d.Candidate = &r
		} else {
			d.Baseline = &r
		}
	}
	if err := rows.Err(); err != nil {
		return RequestDetail{}, err
	}

	diffRows, err := s.pool.Query(ctx, `
		SELECT request_id, endpoint, severity, kind, json_path, baseline_value, candidate_value
		FROM diffs WHERE run_id = $1::uuid AND request_id = $2
		ORDER BY CASE severity WHEN 'breaking' THEN 0 WHEN 'additive' THEN 1 ELSE 2 END, json_path`, runID, requestID)
	if err != nil {
		return RequestDetail{}, fmt.Errorf("load request diffs: %w", err)
	}
	defer diffRows.Close()
	d.Diffs = []DiffRow{}
	for diffRows.Next() {
		var x DiffRow
		if err := diffRows.Scan(&x.RequestID, &x.Endpoint, &x.Severity, &x.Kind, &x.JSONPath, &x.Baseline, &x.Candidate); err != nil {
			return RequestDetail{}, fmt.Errorf("scan request diff: %w", err)
		}
		d.Diffs = append(d.Diffs, x)
	}
	return d, diffRows.Err()
}
