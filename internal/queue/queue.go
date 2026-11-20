// Package queue is a small Postgres-backed job queue.
//
// Workers claim jobs with SELECT ... FOR UPDATE SKIP LOCKED, so any number of
// workers can poll the same table without blocking each other or receiving the
// same job twice. A claim takes a lease; if a worker dies, the lease expires
// and another worker picks the job up again (at-least-once delivery), until
// the job has used all of its attempts and is parked as "dead".
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Job kinds. They match the CHECK constraint on jobs.kind.
const (
	KindReplayBatch = "replay_batch"
	KindDiffRun     = "diff_run"
)

// ErrLeaseLost means the caller no longer holds the job: the lease expired and
// the job was reclaimed, or it was already finished. The caller must discard
// its work rather than report it.
var ErrLeaseLost = errors.New("job lease lost")

// DBTX is satisfied by *pgxpool.Pool and pgx.Tx, so queue operations can join
// a caller's transaction (enqueue and state change commit together).
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewJob describes a job to enqueue.
type NewJob struct {
	ProjectID   string
	RunID       string
	Kind        string
	Payload     any
	MaxAttempts int // 0 means the table default (5)
}

// Job is a claimed job.
type Job struct {
	ID          int64
	ProjectID   string
	RunID       string
	Kind        string
	Payload     json.RawMessage
	Attempts    int // including this attempt
	MaxAttempts int
}

// Filter narrows which jobs a worker will claim.
type Filter struct {
	Kind      string // required
	ProjectID string // empty means any project
	Variant   string // for replay_batch: payload->>'variant'; empty means any
}

// Enqueue adds a job and returns its id.
func Enqueue(ctx context.Context, db DBTX, j NewJob) (int64, error) {
	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return 0, fmt.Errorf("encode job payload: %w", err)
	}
	max := j.MaxAttempts
	if max <= 0 {
		max = 5
	}
	var id int64
	err = db.QueryRow(ctx, `
		INSERT INTO jobs (project_id, run_id, kind, payload, max_attempts)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5)
		RETURNING id`, j.ProjectID, j.RunID, j.Kind, payload, max).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("enqueue job: %w", err)
	}
	return id, nil
}

// Claim leases the next runnable job for workerID, or returns (nil, nil) when
// nothing is runnable. A job is runnable when it is queued and due, or when it
// is running but its lease has expired.
func Claim(ctx context.Context, db DBTX, f Filter, workerID string, lease time.Duration) (*Job, error) {
	if f.Kind == "" {
		return nil, errors.New("claim: filter kind is required")
	}
	// Park jobs whose last holder died after the final attempt; otherwise a
	// poison job would be reclaimed forever.
	if _, err := db.Exec(ctx, `
		UPDATE jobs SET status = 'dead', finished_at = now(),
		       last_error = COALESCE(last_error, 'lease expired after final attempt')
		WHERE kind = $1 AND status = 'running'
		  AND lease_expires_at < now() AND attempts >= max_attempts`, f.Kind); err != nil {
		return nil, fmt.Errorf("claim: reap: %w", err)
	}

	var j Job
	err := db.QueryRow(ctx, `
		WITH next AS (
			SELECT id FROM jobs
			WHERE kind = $1
			  AND ($2 = '' OR project_id::text = $2)
			  AND ($3 = '' OR payload->>'variant' = $3)
			  AND (
			        (status = 'queued'  AND run_after <= now())
			     OR (status = 'running' AND lease_expires_at < now() AND attempts < max_attempts)
			  )
			ORDER BY run_after, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE jobs j SET
			status = 'running',
			attempts = j.attempts + 1,
			locked_by = $4,
			lease_expires_at = now() + make_interval(secs => $5)
		FROM next
		WHERE j.id = next.id
		RETURNING j.id, j.project_id::text, j.run_id::text, j.kind, j.payload, j.attempts, j.max_attempts`,
		f.Kind, f.ProjectID, f.Variant, workerID, lease.Seconds()).
		Scan(&j.ID, &j.ProjectID, &j.RunID, &j.Kind, &j.Payload, &j.Attempts, &j.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return &j, nil
}

// Complete marks a job done. It returns ErrLeaseLost if workerID no longer
// holds the job.
func Complete(ctx context.Context, db DBTX, jobID int64, workerID string) error {
	tag, err := db.Exec(ctx, `
		UPDATE jobs SET status = 'done', finished_at = now(),
		       lease_expires_at = NULL, last_error = NULL
		WHERE id = $1 AND status = 'running' AND locked_by = $2 AND lease_expires_at >= now()`,
		jobID, workerID)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Fail records an error. The job is retried after an exponential backoff
// (base * attempts^2) until it has used all attempts, then parked as dead.
// It reports whether the job will be retried.
func Fail(ctx context.Context, db DBTX, jobID int64, workerID, msg string, backoffBase time.Duration) (retry bool, err error) {
	var status string
	err = db.QueryRow(ctx, `
		UPDATE jobs SET
			status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
			run_after = now() + make_interval(secs => $4 * attempts * attempts),
			finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
			lease_expires_at = NULL,
			locked_by = NULL,
			last_error = $3
		WHERE id = $1 AND status = 'running' AND locked_by = $2
		RETURNING status`,
		jobID, workerID, truncate(msg, 2000), backoffBase.Seconds()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLeaseLost
	}
	if err != nil {
		return false, fmt.Errorf("fail job: %w", err)
	}
	return status == "queued", nil
}

// Extend pushes the lease of a running job forward. Long jobs call it as a
// heartbeat so they are not reclaimed while still alive.
func Extend(ctx context.Context, db DBTX, jobID int64, workerID string, lease time.Duration) error {
	tag, err := db.Exec(ctx, `
		UPDATE jobs SET lease_expires_at = now() + make_interval(secs => $3)
		WHERE id = $1 AND status = 'running' AND locked_by = $2 AND lease_expires_at >= now()`,
		jobID, workerID, lease.Seconds())
	if err != nil {
		return fmt.Errorf("extend lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Remaining counts a run's jobs of one kind that are not finished
// (queued, running or dead). Dead jobs count: a run with a dead batch must
// never look complete.
func Remaining(ctx context.Context, db DBTX, runID, kind string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE run_id = $1::uuid AND kind = $2 AND status <> 'done'`, runID, kind).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count remaining jobs: %w", err)
	}
	return n, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
