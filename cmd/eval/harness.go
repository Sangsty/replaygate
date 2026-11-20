package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/Sangsty/replaygate/internal/diffworker"
	"github.com/Sangsty/replaygate/internal/evalapp"
	"github.com/Sangsty/replaygate/internal/gate"
	"github.com/Sangsty/replaygate/internal/model"
	"github.com/Sangsty/replaygate/internal/recorder"
	"github.com/Sangsty/replaygate/internal/report"
	"github.com/Sangsty/replaygate/internal/server"
	"github.com/Sangsty/replaygate/internal/store"
)

// harness is the real replaygate API and diff worker running in this process
// against a real Postgres. Only the two services under test are fakes.
type harness struct {
	st     *store.Store
	apiURL string
	userID string
	conc   int
}

type project struct{ ID, Key string }

// diffPoll is how often the in-process diff worker looks for work. The
// production default is one second; a shorter interval keeps idle polling out
// of the timings, which are meant to show replay and diff cost.
const diffPoll = 20 * time.Millisecond

func newHarness(ctx context.Context, dbURL string, conc int, log *slog.Logger) (*harness, func(), error) {
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return nil, nil, err
	}
	if err := store.Migrate(ctx, st.Pool()); err != nil {
		st.Close()
		return nil, nil, err
	}
	uid, err := st.UpsertUser(ctx, 0, "eval")
	if err != nil {
		st.Close()
		return nil, nil, err
	}

	dctx, stopDiff := context.WithCancel(ctx)
	diffDone := make(chan struct{})
	dw := &diffworker.Worker{Store: st, Log: log, ID: "eval", Interval: diffPoll}
	go func() { defer close(diffDone); dw.Run(dctx) }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		stopDiff()
		<-diffDone
		st.Close()
		return nil, nil, err
	}
	srv := &http.Server{Handler: server.New(st, log), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	h := &harness{st: st, apiURL: "http://" + ln.Addr().String(), userID: uid, conc: conc}
	cleanup := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		stopDiff()
		<-diffDone
		st.Close()
	}
	return h, cleanup, nil
}

// newProject creates an empty project with its own API key, so each experiment
// starts from a corpus nothing else has touched.
func (h *harness) newProject(ctx context.Context, label string) (project, error) {
	p, err := h.st.CreateProject(ctx, h.userID, fmt.Sprintf("eval-%s-%d", label, time.Now().UnixNano()))
	if err != nil {
		return project{}, err
	}
	key, err := h.st.CreateAPIKey(ctx, p.ID, "eval")
	if err != nil {
		return project{}, err
	}
	return project{ID: p.ID, Key: key}, nil
}

// ingest records targets ("/path" or "/path?query") exactly as the proxy would
// upload them.
func (h *harness) ingest(ctx context.Context, p project, targets []string) error {
	cl := recorder.NewClient(h.apiURL, p.ID, p.Key)
	const batch = 500 // the API's per-call maximum
	for start := 0; start < len(targets); start += batch {
		end := start + batch
		if end > len(targets) {
			end = len(targets)
		}
		reqs := make([]model.RecordedRequest, 0, end-start)
		for _, t := range targets[start:end] {
			path, query, _ := strings.Cut(t, "?")
			reqs = append(reqs, model.RecordedRequest{
				Method: "GET", Path: path, Query: query,
				Headers: map[string][]string{"Accept": {"application/json"}},
				Status:  200, LatencyMS: 5,
			})
		}
		if err := cl.Upload(ctx, reqs); err != nil {
			return fmt.Errorf("ingest: %w", err)
		}
	}
	return nil
}

type scenario struct {
	baseline, candidate evalapp.Options
	noiseLearning       bool
	sample              int // 0 = API default
}

// check runs the gate exactly as the GitHub Action would: replay against a
// baseline and a candidate build, then diff.
func (h *harness) check(ctx context.Context, p project, s scenario) (report.Result, error) {
	b := httptest.NewServer(evalapp.New(s.baseline))
	defer b.Close()
	c := httptest.NewServer(evalapp.New(s.candidate))
	defer c.Close()
	bu, err := url.Parse(b.URL)
	if err != nil {
		return report.Result{}, err
	}
	cu, err := url.Parse(c.URL)
	if err != nil {
		return report.Result{}, err
	}
	nl := s.noiseLearning
	return gate.Run(ctx, gate.Options{
		APIURL: h.apiURL, ProjectID: p.ID, APIKey: p.Key,
		BaselineURL: bu, CandidateURL: cu, CommitSHA: "e0a1000",
		NoiseLearning: &nl, SampleSize: s.sample, Concurrency: h.conc,
		PollEvery: 20 * time.Millisecond, Timeout: 15 * time.Minute,
	})
}
