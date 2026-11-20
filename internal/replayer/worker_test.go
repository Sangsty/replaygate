package replayer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeAPI serves the three worker endpoints from a queue of canned jobs.
type fakeAPI struct {
	mu          sync.Mutex
	jobs        []Job
	claims      int
	completed   map[int64][]Result
	failed      map[int64]string
	completeErr int // status to answer /complete with; 0 means 204
	completeMsg string
	claimStatus int // status to answer /claim with when > 0
	authSeen    string
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects/p1/jobs/claim", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.claims++
		f.authSeen = r.Header.Get("Authorization")
		if f.claimStatus > 0 {
			w.WriteHeader(f.claimStatus)
			_, _ = io.WriteString(w, `{"error":"nope"}`)
			return
		}
		if len(f.jobs) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		j := f.jobs[0]
		f.jobs = f.jobs[1:]
		_ = json.NewEncoder(w).Encode(j)
	})
	mux.HandleFunc("/v1/projects/p1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/projects/p1/jobs/"), "/")
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		var body struct {
			WorkerID string   `json:"worker_id"`
			Results  []Result `json:"results"`
			Error    string   `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch parts[1] {
		case "complete":
			if f.completeErr > 0 {
				w.WriteHeader(f.completeErr)
				_, _ = io.WriteString(w, f.completeMsg)
				return
			}
			f.completed[id] = body.Results
			w.WriteHeader(http.StatusNoContent)
		case "fail":
			f.failed[id] = body.Error
			_ = json.NewEncoder(w).Encode(map[string]bool{"will_retry": true})
		}
	})
	return mux
}

func newAPI(t *testing.T, jobs ...Job) (*fakeAPI, *Client) {
	t.Helper()
	f := &fakeAPI{jobs: jobs, completed: map[int64][]Result{}, failed: map[int64]string{}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, NewClient(srv.URL+"/", "p1", "rg_testkey")
}

func okTarget(t *testing.T) *url.URL {
	return target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"p":"`+r.URL.Path+`"}`) }))
}

func newWorker(c *Client, tgt *url.URL) *Worker {
	return &Worker{
		Client: c, Replayer: &Replayer{Target: tgt}, Log: discard, ID: "w1", Variant: "candidate",
		Poll: 5 * time.Millisecond, IdleExit: 30 * time.Millisecond,
	}
}

func job(id int64, paths ...string) Job {
	j := Job{JobID: id, RunID: "r1", Variant: "candidate"}
	for i, p := range paths {
		j.Requests = append(j.Requests, Request{ID: id*100 + int64(i), Method: "GET", Path: p})
	}
	return j
}

func TestWorkerProcessesJobsThenExitsWhenIdle(t *testing.T) {
	f, c := newAPI(t, job(1, "/a", "/b"), job(2, "/c"))
	st, err := newWorker(c, okTarget(t)).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Jobs != 2 || st.Requests != 3 || st.Failures != 0 {
		t.Errorf("stats = %+v", st)
	}
	if len(f.completed[1]) != 2 || len(f.completed[2]) != 1 {
		t.Fatalf("completed = %v", f.completed)
	}
	if string(f.completed[1][1].Body) != `{"p":"/b"}` || f.completed[1][1].RequestID != 101 {
		t.Errorf("result = %+v", f.completed[1][1])
	}
	if f.authSeen != "Bearer rg_testkey" {
		t.Errorf("API saw Authorization %q", f.authSeen)
	}
}

func TestWorkerCountsTargetFailuresButStillCompletes(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(dead.URL)
	dead.Close()

	f, c := newAPI(t, job(1, "/a", "/b"))
	st, err := newWorker(c, u).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A candidate that is down is a finding, so the batch is reported, not failed.
	if st.Jobs != 1 || st.Failures != 2 || len(f.failed) != 0 {
		t.Errorf("stats = %+v failed = %v", st, f.failed)
	}
	if f.completed[1][0].Error == "" {
		t.Error("per-request error missing from the upload")
	}
}

func TestWorkerStopsOnPermanentAPIError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest} {
		f, c := newAPI(t)
		f.claimStatus = status
		_, err := newWorker(c, okTarget(t)).Run(context.Background())
		if !errors.Is(err, ErrPermanent) {
			t.Errorf("status %d: err = %v, want ErrPermanent", status, err)
		}
		if f.claims != 1 {
			t.Errorf("status %d: worker retried %d times; bad credentials must not be hammered", status, f.claims)
		}
	}
}

func TestWorkerRetriesTransientClaimErrors(t *testing.T) {
	f, c := newAPI(t)
	f.claimStatus = http.StatusServiceUnavailable
	w := newWorker(c, okTarget(t))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := w.Run(ctx); err != nil {
		t.Fatalf("transient errors must not end the worker: %v", err)
	}
	if f.claims < 2 {
		t.Errorf("claims = %d, want a retry", f.claims)
	}
}

func TestWorkerDropsResultsWhenLeaseIsLost(t *testing.T) {
	f, c := newAPI(t, job(1, "/a"))
	f.completeErr = http.StatusConflict
	f.completeMsg = `{"error":"job lease lost: the job was reassigned"}`
	st, err := newWorker(c, okTarget(t)).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Jobs != 0 || len(f.failed) != 0 {
		t.Errorf("stats = %+v failed = %v; a lost lease is not a failure to report", st, f.failed)
	}
}

func TestWorkerReportsRejectedResults(t *testing.T) {
	f, c := newAPI(t, job(1, "/a"))
	f.completeErr = http.StatusBadRequest
	f.completeMsg = `{"error":"request 5 is not part of job 1"}`
	if _, err := newWorker(c, okTarget(t)).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.failed[1], "not part of job") {
		t.Errorf("failed = %v; rejected results must be reported so the run does not wait forever", f.failed)
	}
}

func TestWorkerReleasesJobOnShutdown(t *testing.T) {
	f, c := newAPI(t, job(1, "/slow"))
	slow := target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	w := newWorker(c, slow)
	w.IdleExit = 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = w.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.failed[1], "shut down") {
		t.Errorf("failed = %v; an interrupted batch must be handed back immediately", f.failed)
	}
	if len(f.completed) != 0 {
		t.Error("partial results were uploaded")
	}
}

func TestClientClassifiesStatuses(t *testing.T) {
	tests := []struct {
		status        int
		body          string
		wantLease     bool
		wantPermanent bool
		wantErr       bool
	}{
		{http.StatusNoContent, "", false, false, false},
		{http.StatusConflict, `{"error":"job lease lost"}`, true, false, true},
		{http.StatusConflict, `{"error":"project has no recorded requests"}`, false, true, true},
		{http.StatusUnauthorized, `{}`, false, true, true},
		{http.StatusTooManyRequests, `{}`, false, false, true},
		{http.StatusRequestTimeout, `{}`, false, false, true},
		{http.StatusInternalServerError, `{}`, false, false, true},
	}
	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tt.status)
			_, _ = io.WriteString(w, tt.body)
		}))
		err := NewClient(srv.URL, "p", "k").Complete(context.Background(), "w", 1, nil)
		srv.Close()
		if (err != nil) != tt.wantErr || errors.Is(err, ErrLeaseLost) != tt.wantLease || errors.Is(err, ErrPermanent) != tt.wantPermanent {
			t.Errorf("status %d: err = %v (lease=%v permanent=%v)", tt.status, err,
				errors.Is(err, ErrLeaseLost), errors.Is(err, ErrPermanent))
		}
	}
}
