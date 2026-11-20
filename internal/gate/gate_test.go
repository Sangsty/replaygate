package gate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sangsty/replaygate/internal/report"
)

const (
	pid = "p1"
	rid = "11111111-1111-1111-1111-111111111111"
)

// fakeAPI is just enough of the replaygate API for one check.
type fakeAPI struct {
	mu sync.Mutex

	statuses   []string // returned by successive status polls; the last one repeats
	polls      int
	getFailFor int // fail this many polls with 500 first
	diffs      []report.Diff
	configCode int // 0 = 200
	claimCode  int // 0 = 204 (idle)

	createBody map[string]any
	claimed    map[string]int // variant -> claim calls
	job        map[string]json.RawMessage
	completed  map[string]int
	authSeen   atomic.Value
}

// snapshot copies the fake's counters under its lock. Handlers can still be
// finishing after Run returns (the client has gone, the server goroutine has
// not), so tests must never read the maps directly.
type snapshot struct {
	createBody map[string]any
	claimed    map[string]int
	completed  map[string]int
}

func (f *fakeAPI) snap() snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := snapshot{claimed: map[string]int{}, completed: map[string]int{}}
	for k, v := range f.claimed {
		s.claimed[k] = v
	}
	for k, v := range f.completed {
		s.completed[k] = v
	}
	if f.createBody != nil {
		s.createBody = map[string]any{}
		for k, v := range f.createBody {
			s.createBody[k] = v
		}
	}
	return s
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	base := "/v1/projects/" + pid
	mux.HandleFunc(base+"/config", func(w http.ResponseWriter, r *http.Request) {
		f.authSeen.Store(r.Header.Get("Authorization"))
		if f.configCode != 0 {
			w.WriteHeader(f.configCode)
			return
		}
		_, _ = w.Write([]byte(`{"sanitize_rules":{"headers":["authorization"],"query_params":[],"json_paths":[]},"diff_config":{}}`))
	})
	mux.HandleFunc(base+"/runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&f.createBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run_id":"` + rid + `","status":"replaying","total_requests":2,"jobs":3}`))
	})
	mux.HandleFunc(base+"/runs/"+rid, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.getFailFor > 0 {
			f.getFailFor--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		i := f.polls
		if i >= len(f.statuses) {
			i = len(f.statuses) - 1
		}
		f.polls++
		res := report.Result{Run: report.Run{ID: rid, Status: f.statuses[i], TotalRequests: 2}, Diffs: []report.Diff{}}
		if r.URL.Query().Get("diff_limit") != "0" {
			res.Diffs = f.diffs
			res.Run.Breaking = len(f.diffs)
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc(base+"/jobs/claim", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var req struct{ Variant string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.claimed[req.Variant]++
		if f.claimCode != 0 {
			w.WriteHeader(f.claimCode)
			return
		}
		if raw, ok := f.job[req.Variant]; ok && f.claimed[req.Variant] == 1 {
			_, _ = w.Write(raw)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(base+"/jobs/", func(w http.ResponseWriter, r *http.Request) { // complete
		f.mu.Lock()
		defer f.mu.Unlock()
		parts := strings.Split(r.URL.Path, "/")
		f.completed[parts[len(parts)-2]]++
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func setup(t *testing.T, f *fakeAPI) Options {
	t.Helper()
	f.claimed, f.completed = map[string]int{}, map[string]int{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	base, _ := url.Parse("http://127.0.0.1:1") // nothing listens: replay errors are recorded, not fatal
	return Options{
		APIURL: srv.URL, ProjectID: pid, APIKey: "rg_testkey", BaselineURL: base, CandidateURL: base,
		CommitSHA: "abcdef1", PollEvery: 10 * time.Millisecond, Timeout: 5 * time.Second,
	}
}

func TestRunReturnsTheVerdict(t *testing.T) {
	f := &fakeAPI{statuses: []string{"replaying", "diffing", "failed"},
		diffs: []report.Diff{{RequestID: 1, Endpoint: "/u/{id}", Severity: "breaking", Kind: "field_removed", JSONPath: "$.name"}}}
	o := setup(t, f)
	pr := 42
	o.PRNumber = &pr
	o.SampleSize = 50

	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.Status != "failed" || len(res.Diffs) != 1 || res.Diffs[0].JSONPath != "$.name" {
		t.Errorf("result = %+v", res)
	}
	snap := f.snap()
	if snap.createBody["commit_sha"] != "abcdef1" || snap.createBody["pr_number"] != float64(42) || snap.createBody["sample_size"] != float64(50) {
		t.Errorf("create body = %v", snap.createBody)
	}
	if _, set := snap.createBody["noise_learning"]; set {
		t.Errorf("noise_learning should be left to the API default: %v", snap.createBody)
	}
	if got, _ := f.authSeen.Load().(string); got != "Bearer rg_testkey" {
		t.Errorf("authorization = %q", got)
	}
	for _, v := range []string{"baseline_a", "baseline_b", "candidate"} {
		if snap.claimed[v] == 0 {
			t.Errorf("no worker polled for %s: %v", v, snap.claimed)
		}
	}
}

func TestRunWithoutNoiseLearningStartsTwoWorkers(t *testing.T) {
	f := &fakeAPI{statuses: []string{"passed"}}
	o := setup(t, f)
	off := false
	o.NoiseLearning = &off
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	snap := f.snap()
	if snap.createBody["noise_learning"] != false {
		t.Errorf("create body = %v", snap.createBody)
	}
	if snap.claimed["baseline_b"] != 0 {
		t.Errorf("baseline_b worker should not run when noise learning is off: %v", snap.claimed)
	}
}

// Workers send each variant to the right build and carry the replay credential.
func TestWorkersReplayAgainstTheRightBuild(t *testing.T) {
	var baselineHits, candidateHits atomic.Int32
	var auth atomic.Value
	baseline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		baselineHits.Add(1)
		_, _ = io.WriteString(w, `{"v":1}`)
	}))
	defer baseline.Close()
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidateHits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"v":2}`)
	}))
	defer candidate.Close()

	job := func(variant string, id int) json.RawMessage {
		return json.RawMessage(`{"job_id":` + string(rune('0'+id)) + `,"run_id":"` + rid + `","variant":"` + variant +
			`","requests":[{"id":1,"method":"GET","path":"/ping","query":"","headers":{},"body":null}]}`)
	}
	f := &fakeAPI{statuses: []string{"replaying", "replaying", "replaying", "passed"},
		job: map[string]json.RawMessage{"baseline_a": job("baseline_a", 1), "baseline_b": job("baseline_b", 2), "candidate": job("candidate", 3)}}
	o := setup(t, f)
	o.BaselineURL, _ = url.Parse(baseline.URL)
	o.CandidateURL, _ = url.Parse(candidate.URL)
	o.ReplayAuth = "Bearer replay-token"

	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if baselineHits.Load() != 2 || candidateHits.Load() != 1 {
		t.Errorf("hits: baseline=%d (want 2) candidate=%d (want 1)", baselineHits.Load(), candidateHits.Load())
	}
	if got, _ := auth.Load().(string); got != "Bearer replay-token" {
		t.Errorf("replay credential not sent: %q", got)
	}
	snap := f.snap()
	if len(snap.completed) != 3 {
		t.Errorf("each job should be completed once: %v", snap.completed)
	}
}

func TestRunToleratesFlakyPolls(t *testing.T) {
	f := &fakeAPI{statuses: []string{"passed"}, getFailFor: 3}
	if res, err := Run(context.Background(), setup(t, f)); err != nil || res.Run.Status != "passed" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestRunTimesOut(t *testing.T) {
	f := &fakeAPI{statuses: []string{"replaying"}}
	o := setup(t, f)
	o.Timeout = 150 * time.Millisecond
	_, err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "did not finish") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunFailsFastOnBadCredentials(t *testing.T) {
	// The key is rejected before any run is created.
	f := &fakeAPI{statuses: []string{"passed"}, configCode: http.StatusUnauthorized}
	if _, err := Run(context.Background(), setup(t, f)); err == nil || !strings.Contains(err.Error(), "fetch project settings") {
		t.Fatalf("err = %v", err)
	}
	snap := f.snap()
	if snap.createBody != nil {
		t.Error("a run was created with a bad key")
	}

	// The key is accepted for config but workers are refused: stop, do not wait for the timeout.
	f = &fakeAPI{statuses: []string{"replaying"}, claimCode: http.StatusForbidden}
	o := setup(t, f)
	start := time.Now()
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("took too long to give up on a permanent error")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	f := &fakeAPI{statuses: []string{"replaying"}}
	o := setup(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := Run(ctx, o); err == nil {
		t.Fatal("expected an error after cancellation")
	}
}

func TestGitHubContext(t *testing.T) {
	event := `{"pull_request":{"number":17,"head":{"sha":"deadbeefcafe"}}}`
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	read := func(p string) ([]byte, error) {
		if p == "/event.json" {
			return []byte(event), nil
		}
		return nil, errors.New("no such file")
	}
	tests := []struct {
		name    string
		env     map[string]string
		wantSHA string
		wantPR  int
		wantErr bool
	}{
		{"pull request event", map[string]string{"GITHUB_EVENT_PATH": "/event.json", "GITHUB_SHA": "1234567merge"}, "deadbeefcafe", 17, false},
		{"explicit values win", map[string]string{"GITHUB_EVENT_PATH": "/event.json", "RG_COMMIT_SHA": "abc1234", "RG_PR_NUMBER": "5"}, "abc1234", 5, false},
		{"push event has no PR", map[string]string{"GITHUB_SHA": "0123456789abcdef"}, "0123456789abcdef", 0, false},
		{"unreadable event falls back", map[string]string{"GITHUB_EVENT_PATH": "/missing", "GITHUB_SHA": "abcdef1"}, "abcdef1", 0, false},
		{"no commit at all", map[string]string{}, "", 0, true},
		{"commit is not hex", map[string]string{"RG_COMMIT_SHA": "main"}, "", 0, true},
		{"bad PR number", map[string]string{"RG_COMMIT_SHA": "abc1234", "RG_PR_NUMBER": "x"}, "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// GITHUB_SHA in the first case is not hex on purpose to prove it is not used.
			c, err := GitHubContext(env(tt.env), read)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tt.wantErr {
				return
			}
			if c.CommitSHA != tt.wantSHA {
				t.Errorf("sha = %q, want %q", c.CommitSHA, tt.wantSHA)
			}
			if (c.PRNumber == nil) != (tt.wantPR == 0) || (c.PRNumber != nil && *c.PRNumber != tt.wantPR) {
				t.Errorf("pr = %v, want %d", c.PRNumber, tt.wantPR)
			}
		})
	}
}
