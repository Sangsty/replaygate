package diffworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Sangsty/replaygate/internal/analyzer"
	"github.com/Sangsty/replaygate/internal/diff"
	"github.com/Sangsty/replaygate/internal/queue"
	"github.com/Sangsty/replaygate/internal/store"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeStore struct {
	job        *queue.Job
	input      store.DiffInput
	inputErr   error
	rows       []store.ResponseRow
	rowsErr    error
	saveErr    error
	finishErr  error
	saved      *store.RunVerdict
	finished   bool
	failedWith string
	errored    string
}

func (f *fakeStore) ClaimDiffJob(context.Context, string, time.Duration) (*queue.Job, error) {
	j := f.job
	f.job = nil
	return j, nil
}
func (f *fakeStore) FinishDiffJob(context.Context, int64, string) error {
	f.finished = true
	return f.finishErr
}
func (f *fakeStore) FailDiffJob(_ context.Context, _ int64, _, _, msg string) error {
	f.failedWith = msg
	return nil
}
func (f *fakeStore) RunDiffInput(context.Context, string) (store.DiffInput, error) {
	return f.input, f.inputErr
}
func (f *fakeStore) RunResponses(context.Context, string) ([]store.ResponseRow, error) {
	return f.rows, f.rowsErr
}
func (f *fakeStore) SaveRunResult(_ context.Context, _ string, v store.RunVerdict) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = &v
	return nil
}
func (f *fakeStore) MarkRunErrored(_ context.Context, _, msg string) error {
	f.errored = msg
	return nil
}

func ip(n int) *int { return &n }

func row(req int64, ep, variant, body string) store.ResponseRow {
	return store.ResponseRow{RequestID: req, Endpoint: ep, Variant: variant, Status: ip(200), Body: []byte(body), LatencyMS: 10}
}

func newWorker(fs *fakeStore) *Worker { return &Worker{Store: fs, Log: discard, ID: "w"} }

func baseStore() *fakeStore {
	return &fakeStore{
		job:   &queue.Job{ID: 7, RunID: "run-1", Kind: queue.KindDiffRun},
		input: store.DiffInput{ProjectID: "p", Status: "diffing", Config: []byte(`{}`)},
	}
}

func TestNoJobMeansNoWork(t *testing.T) {
	fs := &fakeStore{}
	worked, err := newWorker(fs).RunOnce(context.Background())
	if worked || err != nil {
		t.Errorf("RunOnce = %v, %v; want false, nil", worked, err)
	}
}

func TestSuccessfulRunStoresVerdictAndFinishesJob(t *testing.T) {
	fs := baseStore()
	fs.rows = []store.ResponseRow{
		row(1, "/o/{id}", "baseline_a", `{"ts":1,"total":10}`),
		row(1, "/o/{id}", "baseline_b", `{"ts":2,"total":10}`),
		row(1, "/o/{id}", "candidate", `{"ts":3,"total":11}`),
	}
	worked, err := newWorker(fs).RunOnce(context.Background())
	if !worked || err != nil {
		t.Fatalf("RunOnce = %v, %v", worked, err)
	}
	if fs.saved == nil || fs.saved.Passed || fs.saved.Breaking != 1 {
		t.Fatalf("saved = %+v, want a failed verdict with 1 breaking finding", fs.saved)
	}
	if len(fs.saved.Diffs) != 1 || fs.saved.Diffs[0].JSONPath != "$.total" {
		t.Errorf("diffs = %+v, want only $.total (ts is learned noise)", fs.saved.Diffs)
	}
	if got := fs.saved.Noise["/o/{id}"]; len(got) != 1 || got[0] != "$.ts" {
		t.Errorf("noise = %v", fs.saved.Noise)
	}
	if !fs.finished {
		t.Error("job not finished")
	}
}

func TestPassingRun(t *testing.T) {
	fs := baseStore()
	fs.rows = []store.ResponseRow{
		row(1, "/x", "baseline_a", `{"a":1}`), row(1, "/x", "baseline_b", `{"a":1}`), row(1, "/x", "candidate", `{"a":1}`),
	}
	if _, err := newWorker(fs).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fs.saved == nil || !fs.saved.Passed {
		t.Errorf("saved = %+v, want passed", fs.saved)
	}
}

func TestAlreadyDecidedRunIsNotReanalyzed(t *testing.T) {
	for _, status := range []string{"passed", "failed"} {
		fs := baseStore()
		fs.input.Status = status
		if _, err := newWorker(fs).RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if fs.saved != nil || !fs.finished {
			t.Errorf("status %s: saved=%v finished=%v; want no save and a finished job", status, fs.saved, fs.finished)
		}
	}
}

func TestBadConfigErrorsTheRunWithoutRetry(t *testing.T) {
	fs := baseStore()
	fs.input.Config = []byte(`{"fail_on":["nonsense"]}`)
	if _, err := newWorker(fs).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fs.errored == "" || !fs.finished || fs.failedWith != "" {
		t.Errorf("errored=%q finished=%v failedWith=%q; want run errored and job finished, not retried", fs.errored, fs.finished, fs.failedWith)
	}
}

func TestTransientFailureIsRetried(t *testing.T) {
	fs := baseStore()
	fs.rowsErr = errors.New("connection reset")
	if _, err := newWorker(fs).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fs.failedWith == "" || fs.finished || fs.errored != "" {
		t.Errorf("failedWith=%q finished=%v errored=%q; want the job failed for retry", fs.failedWith, fs.finished, fs.errored)
	}
}

func TestLostLeaseOnFinishIsTolerated(t *testing.T) {
	fs := baseStore()
	fs.rows = []store.ResponseRow{row(1, "/x", "baseline_a", `{}`), row(1, "/x", "candidate", `{}`)}
	fs.finishErr = queue.ErrLeaseLost
	if _, err := newWorker(fs).RunOnce(context.Background()); err != nil {
		t.Errorf("lost lease should not be an error, the result is already stored: %v", err)
	}
	if fs.saved == nil {
		t.Error("result was not stored")
	}
}

func TestRunStopsWhenContextCancelled(t *testing.T) {
	fs := &fakeStore{}
	w := newWorker(fs)
	w.Interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestObservationsGroupVariantsPerRequest(t *testing.T) {
	failed := store.ResponseRow{RequestID: 2, Endpoint: "/y", Variant: "candidate", Error: "timeout"}
	obs := Observations([]store.ResponseRow{
		row(1, "/x", "baseline_a", `{}`), row(1, "/x", "baseline_b", `{}`), row(1, "/x", "candidate", `{}`), failed,
	})
	if len(obs) != 2 {
		t.Fatalf("observations = %d, want 2", len(obs))
	}
	if obs[0].A == nil || obs[0].B == nil || obs[0].Candidate == nil || obs[0].Endpoint != "/x" {
		t.Errorf("request 1 not fully grouped: %+v", obs[0])
	}
	if obs[1].Candidate == nil || obs[1].Candidate.Err != "timeout" || obs[1].Candidate.Status != 0 {
		t.Errorf("failed response not carried as an error: %+v", obs[1].Candidate)
	}
}

func TestDiffRowsAreSeverityOrderedAndCapped(t *testing.T) {
	var fs []analyzer.RequestFinding
	for i := 0; i < MaxStoredDiffs+10; i++ {
		sev := diff.Additive
		if i == MaxStoredDiffs+5 { // the only breaking finding, last in input order
			sev = diff.Breaking
		}
		fs = append(fs, analyzer.RequestFinding{RequestID: int64(i), Endpoint: "/e",
			Finding: diff.Finding{Severity: sev, Kind: diff.FieldAdded, Path: fmt.Sprintf("$.f%d", i)}})
	}
	rows := DiffRows(fs)
	if len(rows) != MaxStoredDiffs {
		t.Fatalf("rows = %d, want cap %d", len(rows), MaxStoredDiffs)
	}
	if rows[0].Severity != "breaking" {
		t.Errorf("first stored row is %q; the cap must keep the most severe findings", rows[0].Severity)
	}
}

func TestDeadBaselineErrorsTheRunInsteadOfPassing(t *testing.T) {
	fs := baseStore()
	for i := int64(1); i <= 4; i++ {
		fs.rows = append(fs.rows,
			store.ResponseRow{RequestID: i, Endpoint: "/x", Variant: "baseline_a", Error: "connection refused"},
			store.ResponseRow{RequestID: i, Endpoint: "/x", Variant: "baseline_b", Error: "connection refused"},
			row(i, "/x", "candidate", `{"a":1}`))
	}
	if _, err := newWorker(fs).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fs.saved != nil {
		t.Errorf("a verdict was saved (%+v) although the baseline never answered", fs.saved)
	}
	if fs.errored == "" || !fs.finished {
		t.Errorf("errored=%q finished=%v; want the run errored with a clear reason", fs.errored, fs.finished)
	}
}
