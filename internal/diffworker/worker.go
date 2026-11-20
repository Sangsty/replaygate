// Package diffworker runs inside the API process. It claims diff jobs from the
// queue, turns a run's replay responses into findings, and stores the verdict.
package diffworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/Sangsty/replaygate/internal/analyzer"
	"github.com/Sangsty/replaygate/internal/diff"
	"github.com/Sangsty/replaygate/internal/queue"
	"github.com/Sangsty/replaygate/internal/store"
)

// MaxStoredDiffs bounds the findings stored per run. Counts on the run stay
// exact; only the individual rows are capped, most severe first.
const MaxStoredDiffs = 5000

// Store is what the worker needs. *store.Store implements it.
type Store interface {
	ClaimDiffJob(ctx context.Context, workerID string, lease time.Duration) (*queue.Job, error)
	FinishDiffJob(ctx context.Context, jobID int64, workerID string) error
	FailDiffJob(ctx context.Context, jobID int64, runID, workerID, msg string) error
	RunDiffInput(ctx context.Context, runID string) (store.DiffInput, error)
	RunResponses(ctx context.Context, runID string) ([]store.ResponseRow, error)
	SaveRunResult(ctx context.Context, runID string, v store.RunVerdict) error
	MarkRunErrored(ctx context.Context, runID, msg string) error
}

// Worker processes diff jobs.
type Worker struct {
	Store    Store
	Log      *slog.Logger
	ID       string
	Lease    time.Duration // default 5 minutes
	Interval time.Duration // poll interval when idle; default 1 second
}

// Run polls until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = time.Second
	}
	for ctx.Err() == nil {
		worked, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("diff worker", "err", err)
		}
		if worked && err == nil {
			continue // more jobs may be waiting; do not sleep
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// RunOnce claims and processes at most one job. It reports whether a job was found.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	lease := w.Lease
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	job, err := w.Store.ClaimDiffJob(ctx, w.ID, lease)
	if err != nil || job == nil {
		return false, err
	}
	log := w.Log.With("job_id", job.ID, "run_id", job.RunID)

	perr := w.process(ctx, job.RunID, log)
	switch {
	case perr == nil:
		if err := w.Store.FinishDiffJob(ctx, job.ID, w.ID); err != nil {
			if errors.Is(err, queue.ErrLeaseLost) {
				log.Warn("lease lost before completion; the result is stored and a retry will repeat it harmlessly")
				return true, nil
			}
			return true, err
		}
		return true, nil
	case errors.Is(perr, errPermanent):
		// Bad configuration will not fix itself: error the run, finish the job.
		log.Error("diff job cannot succeed", "err", perr)
		if err := w.Store.MarkRunErrored(ctx, job.RunID, perr.Error()); err != nil {
			return true, err
		}
		return true, w.Store.FinishDiffJob(ctx, job.ID, w.ID)
	default:
		log.Error("diff job failed", "err", perr)
		return true, w.Store.FailDiffJob(ctx, job.ID, job.RunID, w.ID, perr.Error())
	}
}

var errPermanent = errors.New("permanent")

func (w *Worker) process(ctx context.Context, runID string, log *slog.Logger) error {
	in, err := w.Store.RunDiffInput(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if in.Status == "passed" || in.Status == "failed" {
		return nil // already decided (a retry after a lost lease)
	}
	cfg, err := analyzer.ParseConfig(in.Config)
	if err != nil {
		return fmt.Errorf("%w: %v", errPermanent, err)
	}
	rows, err := w.Store.RunResponses(ctx, runID)
	if err != nil {
		return err
	}

	start := time.Now()
	res := analyzer.Analyze(Observations(rows), cfg)
	if bad, why := res.BaselineUnhealthy(); bad {
		return fmt.Errorf("%w: %s", errPermanent, why)
	}
	verdict := store.RunVerdict{
		Passed: res.Passed, Breaking: res.Breaking, Additive: res.Additive, Latency: res.Latency,
		Noise: res.Noise, Diffs: DiffRows(res.Findings),
	}
	if err := w.Store.SaveRunResult(ctx, runID, verdict); err != nil {
		return err
	}
	log.Info("run analyzed", "passed", res.Passed, "compared", res.Compared,
		"breaking", res.Breaking, "additive", res.Additive, "latency", res.Latency,
		"noise_endpoints", len(res.Noise), "took_ms", time.Since(start).Milliseconds())
	return nil
}

// Observations groups stored responses by request.
func Observations(rows []store.ResponseRow) []analyzer.Observation {
	byReq := map[int64]*analyzer.Observation{}
	var order []int64
	for _, r := range rows {
		o, ok := byReq[r.RequestID]
		if !ok {
			o = &analyzer.Observation{RequestID: r.RequestID, Endpoint: r.Endpoint}
			byReq[r.RequestID] = o
			order = append(order, r.RequestID)
		}
		resp := &diff.Response{Body: r.Body, LatencyMS: r.LatencyMS, Err: r.Error}
		if r.Status != nil {
			resp.Status = *r.Status
		}
		switch r.Variant {
		case store.VariantBaselineA:
			o.A = resp
		case store.VariantBaselineB:
			o.B = resp
		case store.VariantCandidate:
			o.Candidate = resp
		}
	}
	out := make([]analyzer.Observation, 0, len(order))
	for _, id := range order {
		out = append(out, *byReq[id])
	}
	return out
}

// DiffRows converts findings to storage rows, most severe first, capped.
func DiffRows(fs []analyzer.RequestFinding) []store.DiffRow {
	rank := map[diff.Severity]int{diff.Breaking: 0, diff.Additive: 1, diff.Latency: 2}
	sorted := append([]analyzer.RequestFinding(nil), fs...)
	sort.SliceStable(sorted, func(i, j int) bool { return rank[sorted[i].Severity] < rank[sorted[j].Severity] })
	if len(sorted) > MaxStoredDiffs {
		sorted = sorted[:MaxStoredDiffs]
	}
	out := make([]store.DiffRow, len(sorted))
	for i, f := range sorted {
		out[i] = store.DiffRow{
			RequestID: f.RequestID, Endpoint: f.Endpoint, Severity: string(f.Severity),
			Kind: string(f.Kind), JSONPath: f.Path, Baseline: f.Baseline, Candidate: f.Candidate,
		}
	}
	return out
}
