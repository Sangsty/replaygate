// Package gate runs one regression check from a CI job: it creates a run,
// replays the recorded traffic against the baseline and the pull request
// build, waits for the verdict and returns the result.
package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Sangsty/replaygate/internal/recorder"
	"github.com/Sangsty/replaygate/internal/replayer"
	"github.com/Sangsty/replaygate/internal/report"
	"github.com/Sangsty/replaygate/internal/sanitize"
)

// maxDiffsFetched is the most findings the API returns for one run.
const maxDiffsFetched = 1000

// Options configures a check.
type Options struct {
	APIURL    string
	ProjectID string
	APIKey    string

	BaselineURL  *url.URL // the main branch build
	CandidateURL *url.URL // the pull request build
	// ReplayAuth is the Authorization header value the apps expect. It is used
	// for replay only: never uploaded, never stored.
	ReplayAuth string

	CommitSHA     string
	PRNumber      *int
	SampleSize    int   // 0 = API default
	NoiseLearning *bool // nil = API default (on)

	Concurrency    int           // requests in flight per worker; 0 = 8
	RequestTimeout time.Duration // per replayed request; 0 = 10s
	Timeout        time.Duration // whole check; 0 = 15 minutes
	PollEvery      time.Duration // 0 = 2 seconds

	Log *slog.Logger
}

// Run performs the check. It returns a Result for every run that reached a
// verdict, including failed and errored ones. The error is non-nil only when
// the check itself could not be carried out (API unreachable, timeout, ...),
// in which case the Result is empty.
func Run(ctx context.Context, o Options) (report.Result, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Minute
	}
	if o.PollEvery <= 0 {
		o.PollEvery = 2 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 8
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	// Fetch the redaction rules first: if the API or key is wrong this fails
	// before a run exists, and replayed bodies are redacted on this machine.
	rules, err := recorder.NewClient(o.APIURL, o.ProjectID, o.APIKey).Rules(ctx)
	if err != nil {
		return report.Result{}, fmt.Errorf("fetch project settings: %w", err)
	}
	san, err := sanitize.New(rules)
	if err != nil {
		return report.Result{}, fmt.Errorf("project sanitizer rules are invalid: %w", err)
	}

	api := newAPI(o.APIURL, o.ProjectID, o.APIKey)
	runID, jobs, err := api.createRun(ctx, o)
	if err != nil {
		return report.Result{}, err
	}
	o.Log.Info("run created", "run_id", runID, "jobs", jobs)

	variants := []string{"baseline_a", "baseline_b", "candidate"}
	if o.NoiseLearning != nil && !*o.NoiseLearning {
		variants = []string{"baseline_a", "candidate"}
	}

	workCtx, stopWork := context.WithCancel(ctx)
	workerErrs := make(chan error, len(variants))
	done := make(chan struct{}, len(variants))
	for _, v := range variants {
		target := o.BaselineURL
		if v == "candidate" {
			target = o.CandidateURL
		}
		w := &replayer.Worker{
			Client: replayer.NewClient(o.APIURL, o.ProjectID, o.APIKey),
			Replayer: &replayer.Replayer{
				Target: target, AuthHeader: o.ReplayAuth, Sanitizer: san,
				Concurrency: o.Concurrency, Timeout: o.RequestTimeout,
			},
			Log: o.Log.With("variant", v), ID: fmt.Sprintf("gate-%s-%d", v, os.Getpid()), Variant: v,
			Poll: 500 * time.Millisecond,
		}
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := w.Run(workCtx); err != nil && !errors.Is(err, context.Canceled) {
				workerErrs <- fmt.Errorf("%s worker: %w", w.Variant, err)
			}
		}()
	}
	// Stop the workers and wait for them, so no request is in flight when we return.
	defer func() {
		stopWork()
		for range variants {
			<-done
		}
	}()

	tick := time.NewTicker(o.PollEvery)
	defer tick.Stop()
	for first := true; ; first = false {
		if !first { // look once straight away, then on every tick
			select {
			case err := <-workerErrs:
				return report.Result{}, fmt.Errorf("%w (check RG_API_KEY and RG_PROJECT_ID)", err)
			case <-ctx.Done():
				return report.Result{}, fmt.Errorf("run %s did not finish within %s: %w", runID, o.Timeout, ctx.Err())
			case <-tick.C:
			}
		}
		res, err := api.getRun(ctx, runID, 0)
		if err != nil {
			// A flaky poll is not a verdict; keep waiting until the deadline.
			o.Log.Warn("could not read run status; will retry", "err", err)
			continue
		}
		o.Log.Info("run status", "status", res.Run.Status)
		if !res.Run.Terminal() {
			continue
		}
		return api.getRun(ctx, runID, maxDiffsFetched)
	}
}

// api is the slice of the replaygate API the gate needs besides the worker calls.
type api struct {
	base, projectID, key string
	http                 *http.Client
}

func newAPI(base, projectID, key string) *api {
	return &api{base: strings.TrimRight(base, "/"), projectID: projectID, key: key, http: &http.Client{Timeout: 60 * time.Second}}
}

func (a *api) url(path string) string { return a.base + "/v1/projects/" + a.projectID + path }

func (a *api) call(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("call API: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("API %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func (a *api) createRun(ctx context.Context, o Options) (string, int, error) {
	req := map[string]any{"commit_sha": o.CommitSHA}
	if o.PRNumber != nil {
		req["pr_number"] = *o.PRNumber
	}
	if o.SampleSize > 0 {
		req["sample_size"] = o.SampleSize
	}
	if o.NoiseLearning != nil {
		req["noise_learning"] = *o.NoiseLearning
	}
	var out struct {
		RunID string `json:"run_id"`
		Jobs  int    `json:"jobs"`
	}
	if err := a.call(ctx, http.MethodPost, a.url("/runs"), req, &out); err != nil {
		return "", 0, fmt.Errorf("create run: %w", err)
	}
	if out.RunID == "" {
		return "", 0, errors.New("create run: API returned no run id")
	}
	return out.RunID, out.Jobs, nil
}

func (a *api) getRun(ctx context.Context, runID string, diffLimit int) (report.Result, error) {
	var res report.Result
	err := a.call(ctx, http.MethodGet, fmt.Sprintf("%s?diff_limit=%d", a.url("/runs/"+runID), diffLimit), nil, &res)
	return res, err
}
