package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Sangsty/replaygate/internal/store"
)

const (
	runsURL  = "/v1/projects/" + projA + "/runs"
	claimURL = "/v1/projects/" + projA + "/jobs/claim"
	runUUID  = "33333333-3333-3333-3333-333333333333"
)

// runsFake implements the run and job half of the Store interface.
type runsFake struct {
	createParams store.CreateRunParams
	createErr    error
	getErr       error
	run          store.Run
	diffs        []store.DiffRow
	diffLimit    int

	claimErr   error
	claimJob   *store.ReplayJob
	claimArgs  [4]string
	claimLease time.Duration

	completeErr     error
	completeResults []store.ReplayResult
	completeWorker  string
	completeJob     int64

	failErr   error
	failRetry bool
	failMsg   string
}

func (f *runsFake) CreateRun(_ context.Context, p store.CreateRunParams) (store.Run, int, error) {
	f.createParams = p
	if f.createErr != nil {
		return store.Run{}, 0, f.createErr
	}
	return store.Run{ID: runUUID, Status: "replaying", TotalRequests: 7}, 21, nil
}
func (f *runsFake) GetRun(context.Context, string, string) (store.Run, error) { return f.run, f.getErr }
func (f *runsFake) RunDiffs(_ context.Context, _ string, limit int) ([]store.DiffRow, error) {
	f.diffLimit = limit
	return f.diffs, nil
}
func (f *runsFake) ClaimReplayJob(_ context.Context, pid, variant, worker string, lease time.Duration) (*store.ReplayJob, error) {
	f.claimArgs = [4]string{pid, variant, worker, ""}
	f.claimLease = lease
	return f.claimJob, f.claimErr
}
func (f *runsFake) CompleteReplayJob(_ context.Context, _ string, id int64, worker string, res []store.ReplayResult) error {
	f.completeJob, f.completeWorker, f.completeResults = id, worker, res
	return f.completeErr
}
func (f *runsFake) FailReplayJob(_ context.Context, _ string, _ int64, _, msg string) (bool, error) {
	f.failMsg = msg
	return f.failRetry, f.failErr
}

func bearerA() string { return "Bearer " + keyA }

func TestRunEndpointsRequireTheProjectKey(t *testing.T) {
	h := newHandler(newFake())
	for _, ep := range []struct{ method, path string }{
		{"POST", runsURL}, {"GET", runsURL + "/" + runUUID}, {"POST", claimURL},
		{"POST", "/v1/projects/" + projA + "/jobs/1/complete"}, {"POST", "/v1/projects/" + projA + "/jobs/1/fail"},
	} {
		if rec := do(h, ep.method, ep.path, "", `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key = %d, want 401", ep.method, ep.path, rec.Code)
		}
		if rec := do(h, ep.method, ep.path, "Bearer "+keyB, `{}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with another project's key = %d, want 403", ep.method, ep.path, rec.Code)
		}
	}
}

func TestCreateRun(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		storeErr error
		want     int
		check    func(t *testing.T, f *fakeStore, body string)
	}{
		{"defaults applied", `{"commit_sha":"abc1234"}`, nil, http.StatusCreated, func(t *testing.T, f *fakeStore, body string) {
			p := f.createParams
			if p.ProjectID != projA || p.SampleSize != 1000 || p.BatchSize != 100 || p.NoiseLearning != nil {
				t.Errorf("params = %+v", p)
			}
			if !strings.Contains(body, runUUID) || !strings.Contains(body, `"jobs":21`) {
				t.Errorf("body = %s", body)
			}
		}},
		{"explicit values", `{"commit_sha":"ABCDEF0123456789","pr_number":42,"sample_size":50,"batch_size":10,"noise_learning":false}`, nil, http.StatusCreated,
			func(t *testing.T, f *fakeStore, _ string) {
				p := f.createParams
				if *p.PRNumber != 42 || p.SampleSize != 50 || p.BatchSize != 10 || p.NoiseLearning == nil || *p.NoiseLearning {
					t.Errorf("params = %+v", p)
				}
			}},
		{"missing commit", `{}`, nil, http.StatusBadRequest, nil},
		{"commit not hex", `{"commit_sha":"main"}`, nil, http.StatusBadRequest, nil},
		{"commit too short", `{"commit_sha":"abc12"}`, nil, http.StatusBadRequest, nil},
		{"negative PR", `{"commit_sha":"abc1234","pr_number":-1}`, nil, http.StatusBadRequest, nil},
		{"sample too large", `{"commit_sha":"abc1234","sample_size":20001}`, nil, http.StatusBadRequest, nil},
		{"sample negative", `{"commit_sha":"abc1234","sample_size":-5}`, nil, http.StatusBadRequest, nil},
		{"batch too large", `{"commit_sha":"abc1234","batch_size":501}`, nil, http.StatusBadRequest, nil},
		{"unknown field", `{"commit_sha":"abc1234","sneaky":1}`, nil, http.StatusBadRequest, nil},
		{"not JSON", `nope`, nil, http.StatusBadRequest, nil},
		{"no recorded traffic", `{"commit_sha":"abc1234"}`, store.ErrNoRequests, http.StatusConflict, nil},
		{"unknown project", `{"commit_sha":"abc1234"}`, store.ErrNotFound, http.StatusNotFound, nil},
		{"database failure is not leaked", `{"commit_sha":"abc1234"}`, errors.New("pq: secret detail"), http.StatusInternalServerError, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.createErr = tt.storeErr
			rec := do(newHandler(f), "POST", runsURL, bearerA(), tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "secret detail") {
				t.Error("internal error details leaked to the client")
			}
			if tt.check != nil {
				tt.check(t, f, rec.Body.String())
			}
		})
	}
}

func TestGetRun(t *testing.T) {
	f := newFake()
	f.run = store.Run{ID: runUUID, Status: "failed", Breaking: 2}
	f.diffs = []store.DiffRow{{Severity: "breaking", Kind: "field_removed", JSONPath: "$.name"}}
	h := newHandler(f)

	rec := do(h, "GET", runsURL+"/"+runUUID, bearerA(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Run   store.Run       `json:"run"`
		Diffs []store.DiffRow `json:"diffs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Run.Status != "failed" || got.Run.Breaking != 2 || len(got.Diffs) != 1 || f.diffLimit != 100 {
		t.Errorf("got %+v, diff limit %d", got, f.diffLimit)
	}

	if rec := do(h, "GET", runsURL+"/"+runUUID+"?diff_limit=0", bearerA(), ""); !strings.Contains(rec.Body.String(), `"diffs":[]`) {
		t.Errorf("diff_limit=0 should return an empty list, got %s", rec.Body)
	}
	for _, q := range []string{"?diff_limit=-1", "?diff_limit=1001", "?diff_limit=abc"} {
		if rec := do(h, "GET", runsURL+"/"+runUUID+q, bearerA(), ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, rec.Code)
		}
	}
	if rec := do(h, "GET", runsURL+"/not-a-uuid", bearerA(), ""); rec.Code != http.StatusNotFound {
		t.Errorf("malformed run id = %d, want 404", rec.Code)
	}
	f.getErr = store.ErrNotFound
	if rec := do(h, "GET", runsURL+"/"+runUUID, bearerA(), ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", rec.Code)
	}
}

func TestClaimJob(t *testing.T) {
	f := newFake()
	h := newHandler(f)

	// Nothing to do: 204 with no body.
	rec := do(h, "POST", claimURL, bearerA(), `{"worker_id":"w-1","variant":"candidate"}`)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("idle claim = %d %q, want 204 and an empty body", rec.Code, rec.Body)
	}
	if f.claimArgs[0] != projA || f.claimArgs[1] != "candidate" || f.claimArgs[2] != "w-1" || f.claimLease != 5*time.Minute {
		t.Errorf("claim args = %v lease %v", f.claimArgs, f.claimLease)
	}

	f.claimJob = &store.ReplayJob{JobID: 9, RunID: runUUID, Variant: "candidate",
		Requests: []store.ReplayRequest{{ID: 1, Method: "GET", Path: "/users/1"}}}
	rec = do(h, "POST", claimURL, bearerA(), `{"worker_id":"w-1","variant":"candidate","lease_seconds":120}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"job_id":9`) || f.claimLease != 2*time.Minute {
		t.Errorf("claim = %d %s lease %v", rec.Code, rec.Body, f.claimLease)
	}

	bad := []string{
		`{"worker_id":"","variant":"candidate"}`,
		`{"worker_id":"has space","variant":"candidate"}`,
		`{"worker_id":"` + strings.Repeat("x", 101) + `","variant":"candidate"}`,
		`{"worker_id":"w","variant":"production"}`,
		`{"worker_id":"w","variant":"candidate","lease_seconds":5}`,
		`{"worker_id":"w","variant":"candidate","lease_seconds":99999}`,
		`{"worker_id":"w","variant":"candidate","extra":1}`,
	}
	for _, body := range bad {
		if rec := do(h, "POST", claimURL, bearerA(), body); rec.Code != http.StatusBadRequest {
			t.Errorf("claim %s = %d, want 400", body, rec.Code)
		}
	}
	f.claimErr = errors.New("db down")
	if rec := do(h, "POST", claimURL, bearerA(), `{"worker_id":"w","variant":"candidate"}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestCompleteJob(t *testing.T) {
	jobURL := "/v1/projects/" + projA + "/jobs/12/complete"
	body := `{"worker_id":"w-1","results":[{"request_id":5,"status":200,"body":"eyJvayI6dHJ1ZX0=","latency_ms":9},{"request_id":6,"error":"timeout"}]}`

	f := newFake()
	rec := do(newHandler(f), "POST", jobURL, bearerA(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if f.completeJob != 12 || f.completeWorker != "w-1" || len(f.completeResults) != 2 {
		t.Fatalf("store saw job %d worker %q results %d", f.completeJob, f.completeWorker, len(f.completeResults))
	}
	if string(f.completeResults[0].Body) != `{"ok":true}` || f.completeResults[1].Error != "timeout" {
		t.Errorf("results decoded wrong: %+v", f.completeResults)
	}

	tests := []struct {
		name string
		url  string
		body string
		err  error
		want int
	}{
		{"lease lost", jobURL, body, store.ErrLeaseLost, http.StatusConflict},
		{"results rejected", jobURL, body, store.ErrInvalid, http.StatusBadRequest},
		{"unknown job", jobURL, body, store.ErrNotFound, http.StatusNotFound},
		{"bad worker id", jobURL, `{"worker_id":"a b","results":[]}`, nil, http.StatusBadRequest},
		{"non-numeric job id", "/v1/projects/" + projA + "/jobs/abc/complete", body, nil, http.StatusNotFound},
		{"zero job id", "/v1/projects/" + projA + "/jobs/0/complete", body, nil, http.StatusNotFound},
		{"unknown field", jobURL, `{"worker_id":"w","results":[],"x":1}`, nil, http.StatusBadRequest},
		{"db failure", jobURL, body, errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.completeErr = tt.err
			if rec := do(newHandler(f), "POST", tt.url, bearerA(), tt.body); rec.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestCompleteJobRejectsHugeBodies(t *testing.T) {
	huge := `{"worker_id":"w","results":[{"request_id":1,"status":200,"body":"` + strings.Repeat("A", 65<<20) + `"}]}`
	rec := do(newHandler(newFake()), "POST", "/v1/projects/"+projA+"/jobs/1/complete", bearerA(), huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestFailJob(t *testing.T) {
	url := "/v1/projects/" + projA + "/jobs/3/fail"
	f := newFake()
	f.failRetry = true
	rec := do(newHandler(f), "POST", url, bearerA(), `{"worker_id":"w","error":"target refused connection"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"will_retry":true`) || f.failMsg != "target refused connection" {
		t.Errorf("fail = %d %s msg %q", rec.Code, rec.Body, f.failMsg)
	}

	// A missing message gets a default rather than an empty last_error.
	f = newFake()
	do(newHandler(f), "POST", url, bearerA(), `{"worker_id":"w"}`)
	if f.failMsg == "" {
		t.Error("empty failure message was passed through")
	}

	f = newFake()
	f.failErr = store.ErrLeaseLost
	if rec := do(newHandler(f), "POST", url, bearerA(), `{"worker_id":"w","error":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("lease lost = %d, want 409", rec.Code)
	}
	if rec := do(newHandler(newFake()), "POST", url, bearerA(), `{"worker_id":"bad id"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad worker id = %d, want 400", rec.Code)
	}
}
