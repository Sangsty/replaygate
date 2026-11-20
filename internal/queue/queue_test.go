package queue_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Sangsty/replaygate/internal/queue"
	"github.com/Sangsty/replaygate/internal/store"
)

type fixture struct {
	pool      *pgxpool.Pool
	projectID string
	runID     string
}

// newFixture creates a private schema with the real migrations plus one
// project and one run, so jobs can satisfy their foreign keys.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "q_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	cfg, _ := pgxpool.ParseConfig(url)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	f := &fixture{pool: pool}
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (github_id, login) VALUES (1,'u') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO projects (owner_id, name) VALUES ($1::uuid,'p') RETURNING id::text`, userID).Scan(&f.projectID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO replay_runs (project_id, commit_sha) VALUES ($1::uuid,'abc') RETURNING id::text`, f.projectID).Scan(&f.runID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) enqueue(t *testing.T, kind string, payload any) int64 {
	t.Helper()
	id, err := queue.Enqueue(context.Background(), f.pool, queue.NewJob{
		ProjectID: f.projectID, RunID: f.runID, Kind: kind, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) setLease(t *testing.T, id int64, sql string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), "UPDATE jobs SET "+sql+" WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) status(t *testing.T, id int64) (status string, attempts int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT status, attempts FROM jobs WHERE id=$1`, id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	return
}

var replay = queue.Filter{Kind: queue.KindReplayBatch}

func TestClaimOrderAndFields(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.enqueue(t, queue.KindReplayBatch, map[string]any{"variant": "candidate"})
	second := f.enqueue(t, queue.KindReplayBatch, map[string]any{"variant": "candidate"})

	j, err := queue.Claim(ctx, f.pool, replay, "w1", time.Minute)
	if err != nil || j == nil {
		t.Fatalf("claim = %v, %v", j, err)
	}
	if j.ID != first || j.Attempts != 1 || j.MaxAttempts != 5 || j.ProjectID != f.projectID || j.RunID != f.runID {
		t.Errorf("job = %+v, want first job %d with attempts 1", j, first)
	}
	if string(j.Payload) != `{"variant": "candidate"}` {
		t.Errorf("payload = %s", j.Payload)
	}
	j2, _ := queue.Claim(ctx, f.pool, replay, "w2", time.Minute)
	if j2 == nil || j2.ID != second {
		t.Errorf("second claim = %+v, want job %d", j2, second)
	}
	if j3, err := queue.Claim(ctx, f.pool, replay, "w3", time.Minute); err != nil || j3 != nil {
		t.Errorf("claim on empty queue = %v, %v; want nil, nil", j3, err)
	}
}

func TestClaimFilters(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.enqueue(t, queue.KindReplayBatch, map[string]any{"variant": "baseline_a"})
	c := f.enqueue(t, queue.KindReplayBatch, map[string]any{"variant": "candidate"})
	d := f.enqueue(t, queue.KindDiffRun, map[string]any{})

	j, _ := queue.Claim(ctx, f.pool, queue.Filter{Kind: queue.KindReplayBatch, Variant: "candidate"}, "w", time.Minute)
	if j == nil || j.ID != c {
		t.Fatalf("variant filter claimed %+v, want %d", j, c)
	}
	j, _ = queue.Claim(ctx, f.pool, queue.Filter{Kind: queue.KindDiffRun}, "w", time.Minute)
	if j == nil || j.ID != d {
		t.Fatalf("kind filter claimed %+v, want %d", j, d)
	}
	other := "00000000-0000-0000-0000-000000000000"
	if j, _ := queue.Claim(ctx, f.pool, queue.Filter{Kind: queue.KindReplayBatch, ProjectID: other}, "w", time.Minute); j != nil {
		t.Fatalf("claimed a job of another project: %+v", j)
	}
	j, _ = queue.Claim(ctx, f.pool, queue.Filter{Kind: queue.KindReplayBatch, ProjectID: f.projectID}, "w", time.Minute)
	if j == nil || j.ID != a {
		t.Fatalf("project filter claimed %+v, want %d", j, a)
	}
	if _, err := queue.Claim(ctx, f.pool, queue.Filter{}, "w", time.Minute); err == nil {
		t.Error("claim without kind should be rejected")
	}
}

// The core guarantee: many workers polling at once never receive the same job.
func TestConcurrentClaimsNeverDuplicate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const jobs, workers = 60, 8
	for i := 0; i < jobs; i++ {
		f.enqueue(t, queue.KindReplayBatch, map[string]any{"i": i})
	}

	var mu sync.Mutex
	seen := map[int64]string{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		id := "worker-" + string(rune('A'+w))
		go func() {
			defer wg.Done()
			for {
				j, err := queue.Claim(ctx, f.pool, replay, id, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				if prev, dup := seen[j.ID]; dup {
					t.Errorf("job %d claimed by %s and %s", j.ID, prev, id)
				}
				seen[j.ID] = id
				mu.Unlock()
				if err := queue.Complete(ctx, f.pool, j.ID, id); err != nil {
					t.Errorf("complete: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != jobs {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), jobs)
	}
	if n, _ := queue.Remaining(ctx, f.pool, f.runID, queue.KindReplayBatch); n != 0 {
		t.Errorf("remaining = %d, want 0", n)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.enqueue(t, queue.KindReplayBatch, map[string]any{})

	if _, err := queue.Claim(ctx, f.pool, replay, "dead-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	if j, _ := queue.Claim(ctx, f.pool, replay, "other", time.Minute); j != nil {
		t.Fatal("a job with a live lease must not be claimable")
	}

	f.setLease(t, id, "lease_expires_at = now() - interval '1 second'")
	j, err := queue.Claim(ctx, f.pool, replay, "rescuer", time.Minute)
	if err != nil || j == nil || j.ID != id || j.Attempts != 2 {
		t.Fatalf("reclaim = %+v, %v; want job %d attempt 2", j, err, id)
	}
	// The original worker has lost the job and must not be able to report it.
	if err := queue.Complete(ctx, f.pool, id, "dead-worker"); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("complete by old holder = %v, want ErrLeaseLost", err)
	}
	if err := queue.Complete(ctx, f.pool, id, "rescuer"); err != nil {
		t.Errorf("complete by new holder: %v", err)
	}
}

func TestPoisonJobEndsDeadInsteadOfLoopingForever(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.enqueue(t, queue.KindReplayBatch, map[string]any{})
	f.setLease(t, id, "max_attempts = 2")

	for attempt := 1; attempt <= 2; attempt++ {
		j, _ := queue.Claim(ctx, f.pool, replay, "w", time.Minute)
		if j == nil || j.Attempts != attempt {
			t.Fatalf("attempt %d: claim = %+v", attempt, j)
		}
		f.setLease(t, id, "lease_expires_at = now() - interval '1 second'") // worker "crashes"
	}
	if j, _ := queue.Claim(ctx, f.pool, replay, "w", time.Minute); j != nil {
		t.Fatalf("job reclaimed after exhausting attempts: %+v", j)
	}
	if st, _ := f.status(t, id); st != "dead" {
		t.Errorf("status = %q, want dead", st)
	}
	if n, _ := queue.Remaining(ctx, f.pool, f.runID, queue.KindReplayBatch); n != 1 {
		t.Errorf("remaining = %d; a dead job must keep the run incomplete", n)
	}
}

func TestFailRetriesWithBackoffThenDies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.enqueue(t, queue.KindReplayBatch, map[string]any{})
	f.setLease(t, id, "max_attempts = 2")

	j, _ := queue.Claim(ctx, f.pool, replay, "w", time.Minute)
	retry, err := queue.Fail(ctx, f.pool, j.ID, "w", "boom", time.Hour)
	if err != nil || !retry {
		t.Fatalf("first fail = retry %v, err %v; want retry", retry, err)
	}
	if st, _ := f.status(t, id); st != "queued" {
		t.Errorf("status after first fail = %q, want queued", st)
	}
	if j, _ := queue.Claim(ctx, f.pool, replay, "w", time.Minute); j != nil {
		t.Fatal("job claimable before its backoff elapsed")
	}

	f.setLease(t, id, "run_after = now()") // fast-forward the backoff
	j, _ = queue.Claim(ctx, f.pool, replay, "w", time.Minute)
	if j == nil || j.Attempts != 2 {
		t.Fatalf("second claim = %+v", j)
	}
	retry, err = queue.Fail(ctx, f.pool, j.ID, "w", "boom again", time.Hour)
	if err != nil || retry {
		t.Fatalf("final fail = retry %v, err %v; want no retry", retry, err)
	}
	if st, _ := f.status(t, id); st != "dead" {
		t.Errorf("status = %q, want dead", st)
	}
	var lastErr string
	_ = f.pool.QueryRow(ctx, `SELECT last_error FROM jobs WHERE id=$1`, id).Scan(&lastErr)
	if lastErr != "boom again" {
		t.Errorf("last_error = %q", lastErr)
	}
}

func TestFailByNonHolderIsRejected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.enqueue(t, queue.KindReplayBatch, map[string]any{})
	j, _ := queue.Claim(ctx, f.pool, replay, "holder", time.Minute)
	if _, err := queue.Fail(ctx, f.pool, j.ID, "intruder", "x", time.Second); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("fail by non-holder = %v, want ErrLeaseLost", err)
	}
	if err := queue.Complete(ctx, f.pool, j.ID, "intruder"); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("complete by non-holder = %v, want ErrLeaseLost", err)
	}
}

func TestCompleteAfterLeaseExpiredIsRejected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.enqueue(t, queue.KindReplayBatch, map[string]any{})
	j, _ := queue.Claim(ctx, f.pool, replay, "w", time.Minute)
	f.setLease(t, id, "lease_expires_at = now() - interval '1 second'")
	if err := queue.Complete(ctx, f.pool, j.ID, "w"); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("complete with expired lease = %v, want ErrLeaseLost", err)
	}
}

func TestExtendKeepsJobAlive(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.enqueue(t, queue.KindReplayBatch, map[string]any{})
	j, _ := queue.Claim(ctx, f.pool, replay, "w", time.Second)
	if err := queue.Extend(ctx, f.pool, j.ID, "w", time.Hour); err != nil {
		t.Fatal(err)
	}
	var secs float64
	_ = f.pool.QueryRow(ctx, `SELECT extract(epoch FROM lease_expires_at - now()) FROM jobs WHERE id=$1`, id).Scan(&secs)
	if secs < 3000 {
		t.Errorf("lease extended by only %.0fs", secs)
	}
	if err := queue.Extend(ctx, f.pool, j.ID, "someone-else", time.Hour); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("extend by non-holder = %v, want ErrLeaseLost", err)
	}
}

func TestEnqueueJoinsCallersTransaction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, tx, queue.NewJob{ProjectID: f.projectID, RunID: f.runID, Kind: queue.KindDiffRun, Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := queue.Remaining(ctx, f.pool, f.runID, queue.KindDiffRun); n != 0 {
		t.Errorf("rolled-back enqueue left %d jobs behind", n)
	}
}
