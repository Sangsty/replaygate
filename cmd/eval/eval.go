package main

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Sangsty/replaygate/internal/evalapp"
	"github.com/Sangsty/replaygate/internal/report"
)

// Config sizes the experiments.
type Config struct {
	DetectRuns int     // runs per seeded bug and per benign control
	FPRuns     int     // clean-vs-clean runs per false-positive setting
	Rare       float64 // intermittent-noise probability for the last FP setting
	BenchSizes []int   // number of recorded requests per throughput run
	Bugs       []evalapp.Spec
	Benign     []evalapp.Spec
}

// Env records where the numbers came from.
type Env struct {
	When        time.Time `json:"when"`
	Go          string    `json:"go"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	CPUs        int       `json:"cpus"`
	Postgres    string    `json:"postgres"`
	Concurrency int       `json:"concurrency"`
}

// DetectRow is the outcome for one seeded bug.
type DetectRow struct {
	Spec     evalapp.Spec `json:"spec"`
	Runs     int          `json:"runs"`
	Detected int          `json:"detected"` // the run failed
	Located  int          `json:"located"`  // ...and the report names the expected change
}

// BenignRow is the outcome for one change that must not fail the gate.
type BenignRow struct {
	Spec     evalapp.Spec `json:"spec"`
	Runs     int          `json:"runs"`
	Passed   int          `json:"passed"`
	Findings int          `json:"findings"` // all findings across runs, any severity
}

// FPRow is one false-positive setting: the same build compared with itself.
type FPRow struct {
	Label         string  `json:"label"`
	NoiseLearning bool    `json:"noise_learning"`
	Rare          float64 `json:"rare_noise"`
	Runs          int     `json:"runs"`
	Failed        int     `json:"failed"`   // verdict failed: a false alarm
	Errored       int     `json:"errored"`  // no verdict (should be zero)
	CILo          float64 `json:"ci95_low"` // Wilson interval for Failed/Runs
	CIHi          float64 `json:"ci95_high"`
	MeanBreaking  float64 `json:"mean_breaking_per_run"`
	MeanLatency   float64 `json:"mean_latency_per_run"`
	LatencyRuns   int     `json:"runs_with_latency_findings"`
}

// BenchRow is one throughput run.
type BenchRow struct {
	Requests  int           `json:"requests"`
	Replays   int           `json:"replays"` // requests sent to the services: three passes
	Total     time.Duration `json:"total_ns"`
	Replay    time.Duration `json:"replay_ns"`
	Diff      time.Duration `json:"diff_ns"`
	PerSecond float64       `json:"replays_per_second"`
	Verdict   string        `json:"verdict"`
	Breaking  int           `json:"breaking"`
	Latency   int           `json:"latency_findings"`
}

// Results is everything the evaluation measured.
type Results struct {
	Env    Env         `json:"env"`
	Config Config      `json:"config"`
	Bugs   []DetectRow `json:"bugs"`
	Benign []BenignRow `json:"benign"`
	FP     []FPRow     `json:"false_positives"`
	Bench  []BenchRow  `json:"throughput"`
}

func (h *harness) environment(ctx context.Context) Env {
	var pg string
	_ = h.st.Pool().QueryRow(ctx, `SELECT version()`).Scan(&pg)
	if i := strings.Index(pg, " on "); i > 0 {
		pg = pg[:i]
	}
	return Env{
		When: time.Now().UTC(), Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), Postgres: pg, Concurrency: h.conc,
	}
}

// located reports whether the run's findings contain the change spec expects.
func located(res report.Result, s evalapp.Spec) bool {
	for _, d := range res.Diffs {
		if d.Endpoint == s.Endpoint && d.Kind == string(s.Kind) && d.Severity == "breaking" && strings.Contains(d.JSONPath, s.PathHas) {
			return true
		}
	}
	return false
}

// accuracy runs the seeded-bug, benign-control and false-positive experiments
// against one shared corpus of recorded traffic.
func (h *harness) accuracy(ctx context.Context, cfg Config, progress func(string)) ([]DetectRow, []BenignRow, []FPRow, error) {
	p, err := h.newProject(ctx, "accuracy")
	if err != nil {
		return nil, nil, nil, err
	}
	if err := h.ingest(ctx, p, evalapp.Corpus()); err != nil {
		return nil, nil, nil, err
	}

	// Seeded bugs and controls run with always-changing noise only, so a miss or
	// a false alarm here is about the bug, not about intermittent noise.
	var bugs []DetectRow
	for _, b := range cfg.Bugs {
		row := DetectRow{Spec: b}
		for i := 0; i < cfg.DetectRuns; i++ {
			res, err := h.check(ctx, p, scenario{candidate: evalapp.Options{Mutation: b.Name}, noiseLearning: true})
			if err != nil {
				return nil, nil, nil, fmt.Errorf("bug %s: %w", b.Name, err)
			}
			row.Runs++
			if res.Run.Status == "failed" {
				row.Detected++
				if located(res, b) {
					row.Located++
				}
			}
		}
		progress(fmt.Sprintf("bug %-22s detected %d/%d", b.Name, row.Detected, row.Runs))
		bugs = append(bugs, row)
	}

	var benign []BenignRow
	for _, b := range cfg.Benign {
		row := BenignRow{Spec: b}
		for i := 0; i < cfg.DetectRuns; i++ {
			res, err := h.check(ctx, p, scenario{candidate: evalapp.Options{Mutation: b.Name}, noiseLearning: true})
			if err != nil {
				return nil, nil, nil, fmt.Errorf("benign %s: %w", b.Name, err)
			}
			row.Runs++
			if res.Run.Status == "passed" {
				row.Passed++
			}
			row.Findings += res.Run.Breaking + res.Run.Additive + res.Run.Latency
		}
		progress(fmt.Sprintf("control %-19s passed %d/%d", b.Name, row.Passed, row.Runs))
		benign = append(benign, row)
	}

	settings := []struct {
		label string
		nl    bool
		rare  float64
	}{
		{"noise learning off", false, 0},
		{"noise learning on", true, 0},
		{fmt.Sprintf("noise learning on, plus a field that flips %.1f%% of the time", cfg.Rare*100), true, cfg.Rare},
	}
	var fps []FPRow
	for _, s := range settings {
		row := FPRow{Label: s.label, NoiseLearning: s.nl, Rare: s.rare}
		var brk, lat int
		for i := 0; i < cfg.FPRuns; i++ {
			opt := evalapp.Options{RareNoise: s.rare}
			res, err := h.check(ctx, p, scenario{baseline: opt, candidate: opt, noiseLearning: s.nl})
			if err != nil {
				return nil, nil, nil, fmt.Errorf("false-positive run: %w", err)
			}
			row.Runs++
			switch res.Run.Status {
			case "failed":
				row.Failed++
			case "errored":
				row.Errored++
			}
			brk += res.Run.Breaking
			lat += res.Run.Latency
			if res.Run.Latency > 0 {
				row.LatencyRuns++
			}
		}
		row.CILo, row.CIHi = wilson(row.Failed, row.Runs)
		row.MeanBreaking = float64(brk) / float64(row.Runs)
		row.MeanLatency = float64(lat) / float64(row.Runs)
		progress(fmt.Sprintf("false alarms (%s): %d/%d", s.label, row.Failed, row.Runs))
		fps = append(fps, row)
	}
	return bugs, benign, fps, nil
}

// bench measures a clean check of n recorded requests: three replay passes
// (baseline twice, candidate once) plus the diff.
func (h *harness) bench(ctx context.Context, n int) (BenchRow, error) {
	p, err := h.newProject(ctx, fmt.Sprintf("bench%d", n))
	if err != nil {
		return BenchRow{}, err
	}
	targets := make([]string, n)
	kinds := []string{"users", "orders", "products"}
	for i := range targets {
		targets[i] = "/" + kinds[i%3] + "/" + strconv.Itoa(i/3+1)
	}
	if err := h.ingest(ctx, p, targets); err != nil {
		return BenchRow{}, err
	}

	start := time.Now()
	var replayEnd atomic.Int64 // when the run left "replaying", relative to start
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				runs, err := h.st.ListRuns(ctx, p.ID, 1)
				if err == nil && len(runs) == 1 && runs[0].Status != "replaying" {
					replayEnd.CompareAndSwap(0, int64(time.Since(start)))
					return
				}
			}
		}
	}()
	res, err := h.check(ctx, p, scenario{noiseLearning: true, sample: n})
	total := time.Since(start)
	close(stop)
	<-done
	if err != nil {
		return BenchRow{}, fmt.Errorf("bench %d: %w", n, err)
	}

	row := BenchRow{Requests: n, Replays: 3 * res.Run.TotalRequests, Total: total, Verdict: res.Run.Status, Breaking: res.Run.Breaking, Latency: res.Run.Latency}
	row.Replay = time.Duration(replayEnd.Load())
	if row.Replay == 0 || row.Replay > total {
		row.Replay = total // too fast to observe the phase change; report it all as replay
	}
	row.Diff = total - row.Replay
	row.PerSecond = float64(row.Replays) / total.Seconds()
	return row, nil
}

// wilson is the 95% Wilson score interval for k successes in n trials. Unlike
// the plain normal interval it behaves at 0/n and n/n, which is where a
// false-alarm rate usually sits.
func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959964
	p, nf := float64(k)/float64(n), float64(n)
	den := 1 + z*z/nf
	centre := p + z*z/(2*nf)
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	lo, hi = (centre-margin)/den, (centre+margin)/den
	return math.Max(lo, 0), math.Min(hi, 1)
}
