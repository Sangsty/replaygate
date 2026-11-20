// Package store is the Postgres access layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Sangsty/replaygate/internal/auth"
	"github.com/Sangsty/replaygate/internal/model"
	"github.com/Sangsty/replaygate/internal/normalize"
)

var (
	// ErrNotFound means the requested row does not exist (or is revoked).
	ErrNotFound = errors.New("not found")
	// ErrConflict means a unique constraint would be violated.
	ErrConflict = errors.New("already exists")
)

// uniqueViolation is the Postgres SQLSTATE for unique_violation.
const uniqueViolation = "23505"

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an existing pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying pool, for migrations.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases all connections.
func (s *Store) Close() { s.pool.Close() }

// Project is a row of the projects table (without its JSON configuration).
type Project struct {
	ID      string
	OwnerID string
	Name    string
}

// UpsertUser creates or updates the user with the given GitHub id and returns the user's id.
func (s *Store) UpsertUser(ctx context.Context, githubID int64, login string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (github_id, login) VALUES ($1, $2)
		ON CONFLICT (github_id) DO UPDATE SET login = EXCLUDED.login
		RETURNING id::text`, githubID, login).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("upsert user: %w", err)
	}
	return id, nil
}

// CreateProject creates a project with default sanitizer and diff configuration.
// It returns ErrConflict if the owner already has a project with that name.
func (s *Store) CreateProject(ctx context.Context, ownerID, name string) (Project, error) {
	p := Project{OwnerID: ownerID, Name: name}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO projects (owner_id, name) VALUES ($1::uuid, $2)
		RETURNING id::text`, ownerID, name).Scan(&p.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return Project{}, ErrConflict
		}
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// CreateAPIKey issues a new key for a project. The plaintext is returned once
// and only its hash is stored, so it cannot be recovered later.
func (s *Store) CreateAPIKey(ctx context.Context, projectID, name string) (string, error) {
	key, err := auth.NewKey()
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO api_keys (project_id, name, prefix, key_hash)
		VALUES ($1::uuid, $2, $3, $4)`, projectID, name, key.Prefix, key.Hash)
	if err != nil {
		return "", fmt.Errorf("create api key: %w", err)
	}
	return key.Plaintext, nil
}

// ProjectByAPIKey resolves a plaintext API key to its project id. Revoked and
// unknown keys both return ErrNotFound.
func (s *Store) ProjectByAPIKey(ctx context.Context, plaintext string) (string, error) {
	var projectID string
	err := s.pool.QueryRow(ctx, `
		SELECT project_id::text FROM api_keys
		WHERE key_hash = $1 AND revoked_at IS NULL`, auth.Hash(plaintext)).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("look up api key: %w", err)
	}
	return projectID, nil
}

// ProjectConfig is a project's JSON configuration, kept raw so the API can pass
// it through and each consumer decodes the part it understands.
type ProjectConfig struct {
	SanitizeRules json.RawMessage
	DiffConfig    json.RawMessage
}

// ProjectConfig returns the project's sanitizer rules and diff configuration.
func (s *Store) ProjectConfig(ctx context.Context, projectID string) (ProjectConfig, error) {
	var c ProjectConfig
	err := s.pool.QueryRow(ctx, `
		SELECT sanitize_rules, diff_config FROM projects WHERE id = $1::uuid`, projectID).
		Scan(&c.SanitizeRules, &c.DiffConfig)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectConfig{}, ErrNotFound
	}
	if err != nil {
		return ProjectConfig{}, fmt.Errorf("load project config: %w", err)
	}
	return c, nil
}

const upsertRequestSQL = `
	INSERT INTO recorded_requests
		(project_id, fingerprint, method, path, endpoint, query, headers, body, status, latency_ms)
	VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	ON CONFLICT (project_id, fingerprint) DO UPDATE SET
		hit_count    = recorded_requests.hit_count + 1,
		status       = EXCLUDED.status,
		latency_ms   = EXCLUDED.latency_ms,
		last_seen_at = now()`

// IngestRequests stores a batch of recorded requests atomically. The endpoint
// template and fingerprint are computed here, so identical requests (including
// ones whose query parameters are reordered) are stored once and counted in
// hit_count. Status and latency keep the most recent observation.
func (s *Store) IngestRequests(ctx context.Context, projectID string, reqs []model.RecordedRequest) (int, error) {
	if len(reqs) == 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("ingest: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	batch := &pgx.Batch{}
	for _, r := range reqs {
		headers, err := json.Marshal(r.Headers)
		if err != nil {
			return 0, fmt.Errorf("ingest: encode headers: %w", err)
		}
		if r.Headers == nil {
			headers = []byte(`{}`)
		}
		batch.Queue(upsertRequestSQL,
			projectID,
			normalize.Fingerprint(r.Method, r.Path, r.Query, r.Body),
			r.Method,
			r.Path,
			normalize.Endpoint(r.Path),
			r.Query,
			headers,
			r.Body,
			r.Status,
			r.LatencyMS,
		)
	}

	results := tx.SendBatch(ctx, batch)
	for i := range reqs {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return 0, fmt.Errorf("ingest: request %d: %w", i, err)
		}
	}
	if err := results.Close(); err != nil {
		return 0, fmt.Errorf("ingest: close batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("ingest: commit: %w", err)
	}
	return len(reqs), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
