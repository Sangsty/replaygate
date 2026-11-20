package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Sangsty/replaygate/internal/queue"
)

var (
	// ErrNoRequests means the project has no recorded requests to replay.
	ErrNoRequests = errors.New("project has no recorded requests")
	// ErrInvalid means the caller sent data that does not match the job.
	ErrInvalid = errors.New("invalid request")
	// ErrLeaseLost is returned when a worker reports on a job it no longer holds.
	ErrLeaseLost = queue.ErrLeaseLost
)

// Replay variants. They match the CHECK constraint on replay_responses.variant.
const (
	VariantBaselineA = "baseline_a"
	VariantBaselineB = "baseline_b"
	VariantCandidate = "candidate"
)

// MaxResponseBody is the largest response body stored per replayed request.
const MaxResponseBody = 1 << 20

// CreateRunParams describes a new replay run.
type CreateRunParams struct {
	ProjectID     string
	CommitSHA     string
	PRNumber      *int
	SampleSize    int   // most-hit requests to replay
	BatchSize     int   // requests per replay job
	NoiseLearning *bool // nil keeps the project's setting
}

// Run is a replay run as the API reports it.
type Run struct {
	ID            string     `json:"id"`
	CommitSHA     string     `json:"commit_sha"`
	PRNumber      *int       `json:"pr_number,omitempty"`
	Status        string     `json:"status"`
	TotalRequests int        `json:"total_requests"`
	Breaking      int        `json:"breaking_count"`
	Additive      int        `json:"additive_count"`
	Latency       int        `json:"latency_count"`
	Error         string     `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

type replayPayload struct {
	Variant    string  `json:"variant"`
	RequestIDs []int64 `json:"request_ids"`
}

// CreateRun snapshots the project's most frequently seen requests into a run
// and enqueues the replay jobs: one job per batch per variant. The run and all
// of its jobs are created in one transaction.
func (s *Store) CreateRun(ctx context.Context, p CreateRunParams) (Run, int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, 0, fmt.Errorf("create run: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rawCfg []byte
	err = tx.QueryRow(ctx, `SELECT diff_config FROM projects WHERE id = $1::uuid`, p.ProjectID).Scan(&rawCfg)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, 0, ErrNotFound
	}
	if err != nil {
		return Run{}, 0, fmt.Errorf("create run: load config: %w", err)
	}
	cfg := map[string]json.RawMessage{}
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return Run{}, 0, fmt.Errorf("create run: project diff_config is not an object: %w", err)
	}
	if p.NoiseLearning != nil {
		b, _ := json.Marshal(*p.NoiseLearning)
		cfg["noise_learning"] = b
	}
	noise := true
	if raw, ok := cfg["noise_learning"]; ok {
		_ = json.Unmarshal(raw, &noise)
	}
	runCfg, _ := json.Marshal(cfg)

	rows, err := tx.Query(ctx, `
		SELECT id FROM recorded_requests
		WHERE project_id = $1::uuid
		ORDER BY hit_count DESC, id
		LIMIT $2`, p.ProjectID, p.SampleSize)
	if err != nil {
		return Run{}, 0, fmt.Errorf("create run: select requests: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Run{}, 0, fmt.Errorf("create run: scan request id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Run{}, 0, fmt.Errorf("create run: read requests: %w", err)
	}
	if len(ids) == 0 {
		return Run{}, 0, ErrNoRequests
	}

	run := Run{CommitSHA: p.CommitSHA, PRNumber: p.PRNumber, Status: "replaying", TotalRequests: len(ids)}
	err = tx.QueryRow(ctx, `
		INSERT INTO replay_runs (project_id, commit_sha, pr_number, status, config, total_requests)
		VALUES ($1::uuid, $2, $3, 'replaying', $4, $5)
		RETURNING id::text, created_at`,
		p.ProjectID, p.CommitSHA, p.PRNumber, runCfg, len(ids)).Scan(&run.ID, &run.CreatedAt)
	if err != nil {
		return Run{}, 0, fmt.Errorf("create run: insert: %w", err)
	}

	variants := []string{VariantBaselineA, VariantCandidate}
	if noise {
		variants = append(variants, VariantBaselineB)
	}
	jobs := 0
	for start := 0; start < len(ids); start += p.BatchSize {
		end := start + p.BatchSize
		if end > len(ids) {
			end = len(ids)
		}
		for _, v := range variants {
			_, err := queue.Enqueue(ctx, tx, queue.NewJob{
				ProjectID: p.ProjectID, RunID: run.ID, Kind: queue.KindReplayBatch,
				Payload: replayPayload{Variant: v, RequestIDs: ids[start:end]},
			})
			if err != nil {
				return Run{}, 0, err
			}
			jobs++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Run{}, 0, fmt.Errorf("create run: commit: %w", err)
	}
	return run, jobs, nil
}

// ReplayRequest is a recorded request a worker must replay.
type ReplayRequest struct {
	ID      int64               `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body,omitempty"`
}

// ReplayJob is a leased batch of requests for one variant.
type ReplayJob struct {
	JobID    int64           `json:"job_id"`
	RunID    string          `json:"run_id"`
	Variant  string          `json:"variant"`
	Requests []ReplayRequest `json:"requests"`
}

// ClaimReplayJob leases the next replay batch of the given variant for a
// project. It returns (nil, nil) when there is nothing to do.
func (s *Store) ClaimReplayJob(ctx context.Context, projectID, variant, workerID string, lease time.Duration) (*ReplayJob, error) {
	j, err := queue.Claim(ctx, s.pool, queue.Filter{Kind: queue.KindReplayBatch, ProjectID: projectID, Variant: variant}, workerID, lease)
	if err != nil || j == nil {
		return nil, err
	}
	var payload replayPayload
	if err := json.Unmarshal(j.Payload, &payload); err != nil {
		return nil, s.poison(ctx, j.ID, workerID, "unreadable job payload: "+err.Error())
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, method, path, query, headers, body
		FROM recorded_requests
		WHERE project_id = $1::uuid AND id = ANY($2)
		ORDER BY id`, projectID, payload.RequestIDs)
	if err != nil {
		return nil, fmt.Errorf("load replay requests: %w", err)
	}
	defer rows.Close()
	out := &ReplayJob{JobID: j.ID, RunID: j.RunID, Variant: payload.Variant}
	for rows.Next() {
		var r ReplayRequest
		var headers []byte
		if err := rows.Scan(&r.ID, &r.Method, &r.Path, &r.Query, &headers, &r.Body); err != nil {
			return nil, fmt.Errorf("scan replay request: %w", err)
		}
		if err := json.Unmarshal(headers, &r.Headers); err != nil {
			return nil, fmt.Errorf("decode headers of request %d: %w", r.ID, err)
		}
		out.Requests = append(out.Requests, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read replay requests: %w", err)
	}
	return out, nil
}

// poison fails a job that can never succeed so it does not loop.
func (s *Store) poison(ctx context.Context, jobID int64, workerID, msg string) error {
	_, _ = queue.Fail(ctx, s.pool, jobID, workerID, msg, time.Second)
	return fmt.Errorf("%w: %s", ErrInvalid, msg)
}

// ReplayResult is what a worker observed for one request.
type ReplayResult struct {
	RequestID int64  `json:"request_id"`
	Status    int    `json:"status"`
	Body      []byte `json:"body,omitempty"`
	Truncated bool   `json:"truncated"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// CompleteReplayJob stores a batch of results and marks the job done, in one
// transaction. When it was the run's last replay batch, the diff job is
// enqueued and the run moves to "diffing". Reporting on a job whose lease was
// lost returns ErrLeaseLost and stores nothing.
func (s *Store) CompleteReplayJob(ctx context.Context, projectID string, jobID int64, workerID string, results []ReplayResult) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("complete job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	runID, payload, err := lockHeldJob(ctx, tx, projectID, jobID, workerID, queue.KindReplayBatch)
	if err != nil {
		return err
	}
	var jp replayPayload
	if err := json.Unmarshal(payload, &jp); err != nil {
		return fmt.Errorf("%w: unreadable job payload", ErrInvalid)
	}

	// Serialize completions of the same run. Without this, the last two
	// batches finishing at once would each see the other still "running"
	// (neither has committed), nobody would enqueue the diff job, and the run
	// would hang in "replaying". Lock order is always job row, then run row.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM replay_runs WHERE id = $1::uuid FOR UPDATE`, runID); err != nil {
		return fmt.Errorf("lock run: %w", err)
	}

	// Every request of the batch must be answered exactly once, and nothing else.
	want := make(map[int64]bool, len(jp.RequestIDs))
	for _, id := range jp.RequestIDs {
		want[id] = true
	}
	seen := make(map[int64]bool, len(results))
	for _, r := range results {
		if !want[r.RequestID] {
			return fmt.Errorf("%w: request %d is not part of job %d", ErrInvalid, r.RequestID, jobID)
		}
		if seen[r.RequestID] {
			return fmt.Errorf("%w: request %d reported twice", ErrInvalid, r.RequestID)
		}
		if len(r.Body) > MaxResponseBody {
			return fmt.Errorf("%w: body of request %d exceeds %d bytes", ErrInvalid, r.RequestID, MaxResponseBody)
		}
		seen[r.RequestID] = true
	}
	if len(seen) != len(want) {
		return fmt.Errorf("%w: got %d results, job has %d requests", ErrInvalid, len(seen), len(want))
	}

	batch := &pgx.Batch{}
	for _, r := range results {
		var status *int
		var errMsg *string
		if r.Error != "" {
			e := truncateString(r.Error, 500)
			errMsg = &e
		} else {
			st := r.Status
			status = &st
		}
		batch.Queue(`
			INSERT INTO replay_responses (run_id, request_id, variant, status, body, truncated, latency_ms, error)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (run_id, request_id, variant) DO NOTHING`,
			runID, r.RequestID, jp.Variant, status, r.Body, r.Truncated, r.LatencyMS, errMsg)
	}
	br := tx.SendBatch(ctx, batch)
	for range results {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("store replay response: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("store replay responses: %w", err)
	}

	if err := queue.Complete(ctx, tx, jobID, workerID); err != nil {
		return err
	}

	// If this was the last batch, hand the run to the diff worker. The run
	// lock above makes the count reliable; the "replaying" guard makes the
	// hand-off happen exactly once.
	left, err := queue.Remaining(ctx, tx, runID, queue.KindReplayBatch)
	if err != nil {
		return err
	}
	if left == 0 {
		tag, err := tx.Exec(ctx, `UPDATE replay_runs SET status = 'diffing' WHERE id = $1::uuid AND status = 'replaying'`, runID)
		if err != nil {
			return fmt.Errorf("advance run: %w", err)
		}
		if tag.RowsAffected() == 1 {
			if _, err := queue.Enqueue(ctx, tx, queue.NewJob{ProjectID: projectID, RunID: runID, Kind: queue.KindDiffRun, Payload: map[string]string{}}); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("complete job: commit: %w", err)
	}
	return nil
}

// FailReplayJob reports that a worker could not process a job. The job is
// retried with backoff; when its attempts run out, the run is marked errored.
func (s *Store) FailReplayJob(ctx context.Context, projectID string, jobID int64, workerID, msg string) (retry bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("fail job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	runID, _, err := lockHeldJob(ctx, tx, projectID, jobID, workerID, queue.KindReplayBatch)
	if err != nil {
		return false, err
	}
	retry, err = queue.Fail(ctx, tx, jobID, workerID, msg, 5*time.Second)
	if err != nil {
		return false, err
	}
	if !retry {
		if _, err := tx.Exec(ctx, `
			UPDATE replay_runs SET status = 'errored', error = $2, finished_at = now()
			WHERE id = $1::uuid AND status IN ('pending','replaying')`,
			runID, "replay job failed permanently: "+truncateString(msg, 500)); err != nil {
			return false, fmt.Errorf("mark run errored: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("fail job: commit: %w", err)
	}
	return retry, nil
}

// lockHeldJob locks the job row and verifies it belongs to the project, is of
// the expected kind, and is currently leased to workerID.
func lockHeldJob(ctx context.Context, tx pgx.Tx, projectID string, jobID int64, workerID, kind string) (runID string, payload []byte, err error) {
	var status, lockedBy string
	err = tx.QueryRow(ctx, `
		SELECT run_id::text, payload, status, COALESCE(locked_by, '')
		FROM jobs
		WHERE id = $1 AND project_id = $2::uuid AND kind = $3
		FOR UPDATE`, jobID, projectID, kind).Scan(&runID, &payload, &status, &lockedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("lock job: %w", err)
	}
	if status != "running" || lockedBy != workerID {
		return "", nil, ErrLeaseLost
	}
	return runID, payload, nil
}

// DiffRow is one finding to persist.
type DiffRow struct {
	RequestID int64           `json:"request_id"`
	Endpoint  string          `json:"endpoint"`
	Severity  string          `json:"severity"`
	Kind      string          `json:"kind"`
	JSONPath  string          `json:"json_path"`
	Baseline  json.RawMessage `json:"baseline_value,omitempty"`
	Candidate json.RawMessage `json:"candidate_value,omitempty"`
}

// ResponseRow is one stored replay response joined with its request's endpoint.
type ResponseRow struct {
	RequestID int64
	Endpoint  string
	Variant   string
	Status    *int
	Body      []byte
	LatencyMS int
	Error     string
}

// DiffInput is what the diff worker needs about a run.
type DiffInput struct {
	ProjectID string
	Status    string
	Config    json.RawMessage
}

// RunDiffInput loads a run's status and stored configuration.
func (s *Store) RunDiffInput(ctx context.Context, runID string) (DiffInput, error) {
	var in DiffInput
	err := s.pool.QueryRow(ctx, `
		SELECT project_id::text, status, config FROM replay_runs WHERE id = $1::uuid`, runID).
		Scan(&in.ProjectID, &in.Status, &in.Config)
	if errors.Is(err, pgx.ErrNoRows) {
		return DiffInput{}, ErrNotFound
	}
	if err != nil {
		return DiffInput{}, fmt.Errorf("load run: %w", err)
	}
	return in, nil
}

// RunResponses returns every stored response of a run.
func (s *Store) RunResponses(ctx context.Context, runID string) ([]ResponseRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.request_id, r.endpoint, p.variant, p.status, p.body, COALESCE(p.latency_ms, 0), COALESCE(p.error, '')
		FROM replay_responses p
		JOIN recorded_requests r ON r.id = p.request_id
		WHERE p.run_id = $1::uuid
		ORDER BY p.request_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("load responses: %w", err)
	}
	defer rows.Close()
	var out []ResponseRow
	for rows.Next() {
		var r ResponseRow
		if err := rows.Scan(&r.RequestID, &r.Endpoint, &r.Variant, &r.Status, &r.Body, &r.LatencyMS, &r.Error); err != nil {
			return nil, fmt.Errorf("scan response: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunVerdict is the outcome the diff worker stores.
type RunVerdict struct {
	Passed   bool
	Breaking int
	Additive int
	Latency  int
	Noise    map[string][]string
	Diffs    []DiffRow
}

// SaveRunResult replaces the run's diffs and learned noise and sets its final
// status, in one transaction. It is idempotent, so a retried diff job leaves
// no duplicates.
func (s *Store) SaveRunResult(ctx context.Context, runID string, v RunVerdict) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save result: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, table := range []string{"diffs", "noise_fields"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE run_id = $1::uuid`, runID); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	batch := &pgx.Batch{}
	for ep, paths := range v.Noise {
		for _, p := range paths {
			batch.Queue(`INSERT INTO noise_fields (run_id, endpoint, json_path) VALUES ($1::uuid, $2, $3) ON CONFLICT DO NOTHING`, runID, ep, p)
		}
	}
	for _, d := range v.Diffs {
		batch.Queue(`
			INSERT INTO diffs (run_id, request_id, endpoint, severity, kind, json_path, baseline_value, candidate_value)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT DO NOTHING`,
			runID, d.RequestID, d.Endpoint, d.Severity, d.Kind, d.JSONPath, nullJSON(d.Baseline), nullJSON(d.Candidate))
	}
	if batch.Len() > 0 {
		br := tx.SendBatch(ctx, batch)
		for i := 0; i < batch.Len(); i++ {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return fmt.Errorf("store diff row: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("store diff rows: %w", err)
		}
	}

	status := "failed"
	if v.Passed {
		status = "passed"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE replay_runs SET status = $2, breaking_count = $3, additive_count = $4,
		       latency_count = $5, finished_at = now(), error = NULL
		WHERE id = $1::uuid`, runID, status, v.Breaking, v.Additive, v.Latency); err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("save result: commit: %w", err)
	}
	return nil
}

// MarkRunErrored records that a run could not be completed.
func (s *Store) MarkRunErrored(ctx context.Context, runID, msg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE replay_runs SET status = 'errored', error = $2, finished_at = now()
		WHERE id = $1::uuid AND status NOT IN ('passed','failed')`, runID, truncateString(msg, 500))
	if err != nil {
		return fmt.Errorf("mark run errored: %w", err)
	}
	return nil
}

// GetRun returns a run that belongs to the project.
func (s *Store) GetRun(ctx context.Context, projectID, runID string) (Run, error) {
	var r Run
	var errMsg *string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, commit_sha, pr_number, status, total_requests,
		       breaking_count, additive_count, latency_count, error, created_at, finished_at
		FROM replay_runs WHERE id = $1::uuid AND project_id = $2::uuid`, runID, projectID).
		Scan(&r.ID, &r.CommitSHA, &r.PRNumber, &r.Status, &r.TotalRequests,
			&r.Breaking, &r.Additive, &r.Latency, &errMsg, &r.CreatedAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("load run: %w", err)
	}
	if errMsg != nil {
		r.Error = *errMsg
	}
	return r, nil
}

// RunDiffs returns up to limit findings of a run, most severe first.
func (s *Store) RunDiffs(ctx context.Context, runID string, limit int) ([]DiffRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT request_id, endpoint, severity, kind, json_path, baseline_value, candidate_value
		FROM diffs WHERE run_id = $1::uuid
		ORDER BY CASE severity WHEN 'breaking' THEN 0 WHEN 'additive' THEN 1 ELSE 2 END,
		         endpoint, json_path, request_id
		LIMIT $2`, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("load diffs: %w", err)
	}
	defer rows.Close()
	var out []DiffRow
	for rows.Next() {
		var d DiffRow
		if err := rows.Scan(&d.RequestID, &d.Endpoint, &d.Severity, &d.Kind, &d.JSONPath, &d.Baseline, &d.Candidate); err != nil {
			return nil, fmt.Errorf("scan diff: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ClaimDiffJob leases the next diff job (any project), or returns (nil, nil).
func (s *Store) ClaimDiffJob(ctx context.Context, workerID string, lease time.Duration) (*queue.Job, error) {
	return queue.Claim(ctx, s.pool, queue.Filter{Kind: queue.KindDiffRun}, workerID, lease)
}

// FinishDiffJob marks a diff job done.
func (s *Store) FinishDiffJob(ctx context.Context, jobID int64, workerID string) error {
	return queue.Complete(ctx, s.pool, jobID, workerID)
}

// FailDiffJob records a diff failure. When the job's attempts run out, the run
// is marked errored so a PR check never waits forever.
func (s *Store) FailDiffJob(ctx context.Context, jobID int64, runID, workerID, msg string) error {
	retry, err := queue.Fail(ctx, s.pool, jobID, workerID, msg, 5*time.Second)
	if err != nil {
		return err
	}
	if !retry {
		return s.MarkRunErrored(ctx, runID, "diff failed permanently: "+msg)
	}
	return nil
}
