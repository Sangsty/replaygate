package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Sangsty/replaygate/internal/model"
	"github.com/Sangsty/replaygate/internal/sanitize"
)

// newTestStore returns a Store backed by a private schema, so tests can run in
// parallel and never see each other's rows. It skips when TEST_DATABASE_URL is unset.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(pool)
}

// seedProject creates a user and a project and returns the project id.
func seedProject(t *testing.T, s *Store, name string) string {
	t.Helper()
	ctx := context.Background()
	owner, err := s.UpsertUser(ctx, 1, "tester")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, owner, name)
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := Migrate(ctx, s.Pool()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	files, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if n != len(files) { // one row per migration file, however many there are
		t.Errorf("schema_migrations rows = %d, want %d", n, len(files))
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	projectID := seedProject(t, s, "demo")

	key, err := s.CreateAPIKey(ctx, projectID, "ci")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.ProjectByAPIKey(ctx, key)
	if err != nil || got != projectID {
		t.Fatalf("ProjectByAPIKey = (%q, %v), want (%q, nil)", got, err, projectID)
	}

	if _, err := s.ProjectByAPIKey(ctx, "rg_doesnotexist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key error = %v, want ErrNotFound", err)
	}

	// The plaintext must never be stored.
	var stored int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE prefix = $1 AND key_hash <> $2`,
		key[:8], []byte(key)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Errorf("expected one key row whose hash differs from the plaintext, got %d", stored)
	}

	if _, err := s.Pool().Exec(ctx, `UPDATE api_keys SET revoked_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectByAPIKey(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked key error = %v, want ErrNotFound", err)
	}
}

func TestUpsertUserAndDuplicateProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a, err := s.UpsertUser(ctx, 42, "old-login")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.UpsertUser(ctx, 42, "new-login")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("same github id produced two users: %s vs %s", a, b)
	}
	var login string
	if err := s.Pool().QueryRow(ctx, `SELECT login FROM users WHERE id = $1::uuid`, a).Scan(&login); err != nil {
		t.Fatal(err)
	}
	if login != "new-login" {
		t.Errorf("login = %q, want updated value", login)
	}

	if _, err := s.CreateProject(ctx, a, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, a, "demo"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate project error = %v, want ErrConflict", err)
	}
}

func TestProjectConfigDefaults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	projectID := seedProject(t, s, "demo")

	cfg, err := s.ProjectConfig(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	var rules sanitize.Rules
	if err := json.Unmarshal(cfg.SanitizeRules, &rules); err != nil {
		t.Fatalf("decode sanitize rules: %v", err)
	}
	if len(rules.Headers) == 0 {
		t.Error("default sanitize rules have no headers")
	}
	if _, err := sanitize.New(rules); err != nil {
		t.Errorf("default rules do not compile: %v", err)
	}
	if !json.Valid(cfg.DiffConfig) {
		t.Errorf("diff config is not valid JSON: %s", cfg.DiffConfig)
	}

	if _, err := s.ProjectConfig(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown project error = %v, want ErrNotFound", err)
	}
}

func TestIngestDeduplicatesByFingerprint(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	projectID := seedProject(t, s, "demo")

	first := model.RecordedRequest{
		Method: "GET", Path: "/users/42", Query: "a=1&b=2",
		Headers: map[string][]string{"accept": {"application/json"}},
		Status:  200, LatencyMS: 10,
	}
	reordered := first
	reordered.Query = "b=2&a=1"
	reordered.Status = 500
	reordered.LatencyMS = 99
	other := model.RecordedRequest{
		Method: "GET", Path: "/users/43/orders/7", Status: 404, LatencyMS: 3,
		Body: []byte(`{"q":1}`),
	}

	n, err := s.IngestRequests(ctx, projectID, []model.RecordedRequest{first, reordered, other})
	if err != nil || n != 3 {
		t.Fatalf("IngestRequests = (%d, %v), want (3, nil)", n, err)
	}

	var rows int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM recorded_requests`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("rows = %d, want 2 (reordered query must de-duplicate)", rows)
	}

	var (
		hits, status, latency int
		endpoint              string
		headers               []byte
	)
	err = s.Pool().QueryRow(ctx, `
		SELECT hit_count, status, latency_ms, endpoint, headers
		FROM recorded_requests WHERE path = '/users/42'`).Scan(&hits, &status, &latency, &endpoint, &headers)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 2 || status != 500 || latency != 99 {
		t.Errorf("hit_count/status/latency = %d/%d/%d, want 2/500/99 (latest observation wins)", hits, status, latency)
	}
	if endpoint != "/users/{id}" {
		t.Errorf("endpoint = %q, want /users/{id}", endpoint)
	}
	var h map[string][]string
	if err := json.Unmarshal(headers, &h); err != nil || h["accept"][0] != "application/json" {
		t.Errorf("headers = %s (err %v), want accept preserved", headers, err)
	}

	var body []byte
	var otherEndpoint string
	if err := s.Pool().QueryRow(ctx, `SELECT endpoint, body FROM recorded_requests WHERE path = '/users/43/orders/7'`).
		Scan(&otherEndpoint, &body); err != nil {
		t.Fatal(err)
	}
	if otherEndpoint != "/users/{id}/orders/{id}" || string(body) != `{"q":1}` {
		t.Errorf("second row endpoint/body = %q/%q", otherEndpoint, body)
	}
}

func TestIngestIsAtomicAndRejectsNonGET(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	projectID := seedProject(t, s, "demo")

	good := model.RecordedRequest{Method: "GET", Path: "/ok", Status: 200}
	bad := model.RecordedRequest{Method: "POST", Path: "/nope", Status: 200}

	if _, err := s.IngestRequests(ctx, projectID, []model.RecordedRequest{good, bad}); err == nil {
		t.Fatal("expected the schema CHECK to reject a POST, got nil")
	}
	var rows int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM recorded_requests`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("rows = %d, want 0 (batch must roll back as a whole)", rows)
	}
}

func TestIngestEmptyBatch(t *testing.T) {
	s := newTestStore(t)
	n, err := s.IngestRequests(context.Background(), "00000000-0000-0000-0000-000000000000", nil)
	if err != nil || n != 0 {
		t.Errorf("IngestRequests(nil) = (%d, %v), want (0, nil)", n, err)
	}
}
