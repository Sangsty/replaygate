package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Sangsty/replaygate/internal/model"
)

// seedProjectWithRequests creates a user and project, and records n distinct GET requests.
func seedProjectWithRequests(t *testing.T, s *Store, n int) string {
	t.Helper()
	ctx := context.Background()
	uid, err := s.UpsertUser(ctx, 1, "owner")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, uid, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var reqs []model.RecordedRequest
	for i := 0; i < n; i++ {
		reqs = append(reqs, model.RecordedRequest{
			Method: "GET", Path: fmt.Sprintf("/users/%d", 100+i), Query: "a=1",
			Headers: map[string][]string{"accept": {"application/json"}}, Status: 200, LatencyMS: 5,
		})
	}
	if n > 0 {
		if _, err := s.IngestRequests(ctx, p.ID, reqs); err != nil {
			t.Fatal(err)
		}
	}
	return p.ID
}

func results(job *ReplayJob, status int) []ReplayResult {
	out := make([]ReplayResult, len(job.Requests))
	for i, r := range job.Requests {
		out[i] = ReplayResult{RequestID: r.ID, Status: status, Body: []byte(`{"ok":true}`), LatencyMS: 7}
	}
	return out
}

func runStatus(t *testing.T, s *Store, pid, rid string) Run {
	t.Helper()
	r, err := s.GetRun(context.Background(), pid, rid)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateRunEnqueuesBatchesPerVariant(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 5)

	run, jobs, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 100, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	// 5 requests in batches of 2 = 3 batches, times 3 variants (noise learning is on by default).
	if run.TotalRequests != 5 || jobs != 9 || run.Status != "replaying" {
		t.Errorf("run = %+v jobs = %d, want 5 requests, 9 jobs, replaying", run, jobs)
	}

	_, jobs, err = s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 3, BatchSize: 10, NoiseLearning: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if jobs != 2 { // one batch, baseline_a + candidate only
		t.Errorf("jobs with noise learning off = %d, want 2", jobs)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestCreateRunSamplesMostHitFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 3)
	// Hit /users/102 four more times so it is the most frequent.
	hot := model.RecordedRequest{Method: "GET", Path: "/users/102", Query: "a=1",
		Headers: map[string][]string{"accept": {"application/json"}}, Status: 200}
	for i := 0; i < 4; i++ {
		if _, err := s.IngestRequests(ctx, pid, []model.RecordedRequest{hot}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 1, BatchSize: 10, NoiseLearning: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimReplayJob(ctx, pid, VariantCandidate, "w", time.Minute)
	if err != nil || job == nil || len(job.Requests) != 1 || job.Requests[0].Path != "/users/102" {
		t.Fatalf("job = %+v, err %v; want the single most-hit request /users/102", job, err)
	}
}

func TestCreateRunErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	empty := seedProjectWithRequests(t, s, 0)
	if _, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: empty, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10}); !errors.Is(err, ErrNoRequests) {
		t.Errorf("empty project: err = %v, want ErrNoRequests", err)
	}
	missing := "00000000-0000-0000-0000-000000000000"
	if _, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: missing, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown project: err = %v, want ErrNotFound", err)
	}
}

// The full lifecycle: every batch of every variant completes, the run moves to
// diffing exactly once, and a diff job appears.
func TestRunLifecycleReachesDiffing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 4)
	run, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range []string{VariantBaselineA, VariantBaselineB, VariantCandidate} {
		for batch := 0; batch < 2; batch++ {
			if st := runStatus(t, s, pid, run.ID).Status; st != "replaying" {
				t.Fatalf("status before %s batch %d = %q, want replaying", v, batch, st)
			}
			job, err := s.ClaimReplayJob(ctx, pid, v, "w1", time.Minute)
			if err != nil || job == nil {
				t.Fatalf("claim %s: %v, %v", v, job, err)
			}
			if job.Variant != v || len(job.Requests) != 2 || job.RunID != run.ID {
				t.Fatalf("job = %+v", job)
			}
			if err := s.CompleteReplayJob(ctx, pid, job.JobID, "w1", results(job, 200)); err != nil {
				t.Fatalf("complete: %v", err)
			}
		}
	}
	if st := runStatus(t, s, pid, run.ID).Status; st != "diffing" {
		t.Errorf("status after all batches = %q, want diffing", st)
	}
	var diffJobs int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE run_id=$1::uuid AND kind='diff_run'`, run.ID).Scan(&diffJobs); err != nil {
		t.Fatal(err)
	}
	if diffJobs != 1 {
		t.Errorf("diff jobs = %d, want exactly 1", diffJobs)
	}
	rows, err := s.RunResponses(ctx, run.ID)
	if err != nil || len(rows) != 12 { // 4 requests x 3 variants
		t.Fatalf("responses = %d, err %v; want 12", len(rows), err)
	}
}

// Regression test for the lost-diff-job race: when the last two batches commit
// at the same moment, the diff job must still be enqueued exactly once.
func TestConcurrentFinalBatchesEnqueueDiffOnce(t *testing.T) {
	for round := 0; round < 5; round++ {
		s := newTestStore(t)
		ctx := context.Background()
		pid := seedProjectWithRequests(t, s, 2)
		run, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 1, NoiseLearning: boolPtr(false)})
		if err != nil {
			t.Fatal(err)
		}
		// 2 batches x 2 variants = 4 jobs. Claim all, then complete them at once.
		var claimed []*ReplayJob
		for _, v := range []string{VariantBaselineA, VariantCandidate} {
			for i := 0; i < 2; i++ {
				j, err := s.ClaimReplayJob(ctx, pid, v, "w", time.Minute)
				if err != nil || j == nil {
					t.Fatalf("claim: %v %v", j, err)
				}
				claimed = append(claimed, j)
			}
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, j := range claimed {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := s.CompleteReplayJob(ctx, pid, j.JobID, "w", results(j, 200)); err != nil {
					t.Errorf("complete job %d: %v", j.JobID, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		var diffJobs int
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE run_id=$1::uuid AND kind='diff_run'`, run.ID).Scan(&diffJobs)
		if diffJobs != 1 || runStatus(t, s, pid, run.ID).Status != "diffing" {
			t.Fatalf("round %d: diff jobs = %d, status = %q; want 1 and diffing", round, diffJobs, runStatus(t, s, pid, run.ID).Status)
		}
	}
}

func TestCompleteReplayJobValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 2)
	if _, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10, NoiseLearning: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	job, _ := s.ClaimReplayJob(ctx, pid, VariantCandidate, "w", time.Minute)
	good := results(job, 200)

	tests := []struct {
		name    string
		results []ReplayResult
		want    error
	}{
		{"missing a request", good[:1], ErrInvalid},
		{"unknown request id", append(append([]ReplayResult{}, good...), ReplayResult{RequestID: 999999, Status: 200}), ErrInvalid},
		{"duplicate request", []ReplayResult{good[0], good[0]}, ErrInvalid},
		{"oversized body", []ReplayResult{{RequestID: good[0].RequestID, Body: make([]byte, MaxResponseBody+1)}, good[1]}, ErrInvalid},
		{"empty", nil, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.CompleteReplayJob(ctx, pid, job.JobID, "w", tt.results); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	// A rejected report must leave the job leased so the worker can resubmit.
	if err := s.CompleteReplayJob(ctx, pid, job.JobID, "w", good); err != nil {
		t.Errorf("valid resubmission after rejections failed: %v", err)
	}
}

func TestCompleteReplayJobOwnership(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 1)
	if _, _, err := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10, NoiseLearning: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	job, _ := s.ClaimReplayJob(ctx, pid, VariantCandidate, "holder", time.Minute)

	if err := s.CompleteReplayJob(ctx, pid, job.JobID, "intruder", results(job, 200)); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("wrong worker: err = %v, want ErrLeaseLost", err)
	}
	otherProject := "00000000-0000-0000-0000-000000000000"
	if err := s.CompleteReplayJob(ctx, otherProject, job.JobID, "holder", results(job, 200)); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong project: err = %v, want ErrNotFound", err)
	}
	if err := s.CompleteReplayJob(ctx, pid, job.JobID, "holder", results(job, 200)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteReplayJob(ctx, pid, job.JobID, "holder", results(job, 200)); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("second completion: err = %v, want ErrLeaseLost", err)
	}
}

func TestReplayErrorsAreStoredWithoutStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 1)
	run, _, _ := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10, NoiseLearning: boolPtr(false)})
	job, _ := s.ClaimReplayJob(ctx, pid, VariantCandidate, "w", time.Minute)
	res := []ReplayResult{{RequestID: job.Requests[0].ID, Error: "connection refused", LatencyMS: 3}}
	if err := s.CompleteReplayJob(ctx, pid, job.JobID, "w", res); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.RunResponses(ctx, run.ID)
	if len(rows) != 1 || rows[0].Status != nil || rows[0].Error != "connection refused" {
		t.Errorf("rows = %+v, want one row with an error and no status", rows)
	}
}

func TestFailReplayJobRetriesThenErrorsTheRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 1)
	run, _, _ := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10, NoiseLearning: boolPtr(false)})
	if _, err := s.pool.Exec(ctx, `UPDATE jobs SET max_attempts = 2 WHERE run_id = $1::uuid`, run.ID); err != nil {
		t.Fatal(err)
	}

	job, _ := s.ClaimReplayJob(ctx, pid, VariantCandidate, "w", time.Minute)
	retry, err := s.FailReplayJob(ctx, pid, job.JobID, "w", "target down")
	if err != nil || !retry {
		t.Fatalf("first failure: retry=%v err=%v, want retry", retry, err)
	}
	if st := runStatus(t, s, pid, run.ID).Status; st != "replaying" {
		t.Errorf("run status after a retryable failure = %q, want replaying", st)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE jobs SET run_after = now() WHERE run_id = $1::uuid`, run.ID); err != nil {
		t.Fatal(err)
	}
	job, _ = s.ClaimReplayJob(ctx, pid, VariantCandidate, "w", time.Minute)
	retry, err = s.FailReplayJob(ctx, pid, job.JobID, "w", "target still down")
	if err != nil || retry {
		t.Fatalf("final failure: retry=%v err=%v, want no retry", retry, err)
	}
	got := runStatus(t, s, pid, run.ID)
	if got.Status != "errored" || got.Error == "" || got.FinishedAt == nil {
		t.Errorf("run = %+v, want errored with a message and finish time", got)
	}
}

func TestSaveRunResultIsIdempotentAndSetsVerdict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 1)
	run, _, _ := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10})
	var reqID int64
	_ = s.pool.QueryRow(ctx, `SELECT id FROM recorded_requests LIMIT 1`).Scan(&reqID)

	verdict := RunVerdict{
		Passed: false, Breaking: 1, Additive: 1,
		Noise: map[string][]string{"/users/{id}": {"$.ts"}},
		Diffs: []DiffRow{
			{RequestID: reqID, Endpoint: "/users/{id}", Severity: "breaking", Kind: "field_removed", JSONPath: "$.name", Baseline: []byte(`"a"`)},
			{RequestID: reqID, Endpoint: "/users/{id}", Severity: "additive", Kind: "field_added", JSONPath: "$.age", Candidate: []byte(`3`)},
		},
	}
	for i := 0; i < 2; i++ { // saving twice (a retried diff job) must not duplicate
		if err := s.SaveRunResult(ctx, run.ID, verdict); err != nil {
			t.Fatal(err)
		}
	}
	got := runStatus(t, s, pid, run.ID)
	if got.Status != "failed" || got.Breaking != 1 || got.Additive != 1 || got.FinishedAt == nil {
		t.Errorf("run = %+v", got)
	}
	diffs, err := s.RunDiffs(ctx, run.ID, 10)
	if err != nil || len(diffs) != 2 {
		t.Fatalf("diffs = %d, err %v; want 2", len(diffs), err)
	}
	if diffs[0].Severity != "breaking" || diffs[1].Severity != "additive" {
		t.Errorf("diffs not ordered by severity: %+v", diffs)
	}
	var noise int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM noise_fields WHERE run_id=$1::uuid`, run.ID).Scan(&noise)
	if noise != 1 {
		t.Errorf("noise rows = %d, want 1", noise)
	}

	if err := s.SaveRunResult(ctx, run.ID, RunVerdict{Passed: true}); err != nil {
		t.Fatal(err)
	}
	if got := runStatus(t, s, pid, run.ID); got.Status != "passed" || got.Breaking != 0 {
		t.Errorf("rerun verdict not applied: %+v", got)
	}
	if diffs, _ := s.RunDiffs(ctx, run.ID, 10); len(diffs) != 0 {
		t.Errorf("stale diffs survived a new verdict: %d", len(diffs))
	}
}

func TestGetRunIsScopedToProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 1)
	run, _, _ := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10})
	other := "00000000-0000-0000-0000-000000000000"
	if _, err := s.GetRun(ctx, other, run.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("run visible through another project: err = %v", err)
	}
	if _, err := s.GetRun(ctx, pid, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown run: err = %v", err)
	}
}

func TestMarkRunErroredDoesNotOverrideAVerdict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProjectWithRequests(t, s, 1)
	run, _, _ := s.CreateRun(ctx, CreateRunParams{ProjectID: pid, CommitSHA: "abc1234", SampleSize: 10, BatchSize: 10})
	if err := s.SaveRunResult(ctx, run.ID, RunVerdict{Passed: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRunErrored(ctx, run.ID, "late failure"); err != nil {
		t.Fatal(err)
	}
	if got := runStatus(t, s, pid, run.ID); got.Status != "passed" {
		t.Errorf("status = %q; an errored mark must not overwrite a finished verdict", got.Status)
	}
}
