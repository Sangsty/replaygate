package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sangsty/replaygate/internal/store"
)

const (
	defaultSampleSize = 1000
	maxSampleSize     = 20000
	defaultBatchSize  = 100
	maxBatchSize      = 500
	defaultLease      = 5 * time.Minute
	minLease          = 30 * time.Second
	maxLease          = time.Hour
	// maxCompleteBody fits a default batch (100 responses) at the worker's
	// 256 KiB per-response cap, base64 encoded, with room to spare.
	maxCompleteBody  = 64 << 20
	maxSmallBody     = 64 << 10
	defaultDiffLimit = 100
	maxDiffLimit     = 1000
)

var (
	commitSHARe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	uuidRe      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	workerIDRe  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)
)

func validVariant(v string) bool {
	return v == store.VariantBaselineA || v == store.VariantBaselineB || v == store.VariantCandidate
}

// decodeBody reads a JSON body of at most max bytes into v, rejecting unknown
// fields. It writes the error response itself and reports whether to continue.
func decodeBody(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body larger than %d bytes", max))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// storeError maps store errors to HTTP responses. Unknown errors are logged
// and reported as 500 without leaking details.
func (a *api) storeError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrLeaseLost):
		writeError(w, http.StatusConflict, "job lease lost: the job was reassigned or already finished; discard this work")
	case errors.Is(err, store.ErrNoRequests):
		writeError(w, http.StatusConflict, "project has no recorded requests to replay yet")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		a.log.Error(what, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

type createRunRequest struct {
	CommitSHA     string `json:"commit_sha"`
	PRNumber      *int   `json:"pr_number"`
	SampleSize    int    `json:"sample_size"`
	BatchSize     int    `json:"batch_size"`
	NoiseLearning *bool  `json:"noise_learning"`
}

func (a *api) createRun(w http.ResponseWriter, r *http.Request) {
	var req createRunRequest
	if !decodeBody(w, r, maxSmallBody, &req) {
		return
	}
	if !commitSHARe.MatchString(req.CommitSHA) {
		writeError(w, http.StatusBadRequest, "commit_sha must be 7 to 64 hex characters")
		return
	}
	if req.PRNumber != nil && *req.PRNumber <= 0 {
		writeError(w, http.StatusBadRequest, "pr_number must be positive")
		return
	}
	if req.SampleSize == 0 {
		req.SampleSize = defaultSampleSize
	}
	if req.SampleSize < 0 || req.SampleSize > maxSampleSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("sample_size must be between 1 and %d", maxSampleSize))
		return
	}
	if req.BatchSize == 0 {
		req.BatchSize = defaultBatchSize
	}
	if req.BatchSize < 0 || req.BatchSize > maxBatchSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("batch_size must be between 1 and %d", maxBatchSize))
		return
	}

	run, jobs, err := a.store.CreateRun(r.Context(), store.CreateRunParams{
		ProjectID: chi.URLParam(r, "pid"), CommitSHA: req.CommitSHA, PRNumber: req.PRNumber,
		SampleSize: req.SampleSize, BatchSize: req.BatchSize, NoiseLearning: req.NoiseLearning,
	})
	if err != nil {
		a.storeError(w, "create run", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"run_id": run.ID, "status": run.Status, "total_requests": run.TotalRequests, "jobs": jobs,
	})
}

func (a *api) getRun(w http.ResponseWriter, r *http.Request) {
	rid := chi.URLParam(r, "rid")
	if !uuidRe.MatchString(rid) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	limit := defaultDiffLimit
	if s := r.URL.Query().Get("diff_limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > maxDiffLimit {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("diff_limit must be between 0 and %d", maxDiffLimit))
			return
		}
		limit = n
	}
	run, err := a.store.GetRun(r.Context(), chi.URLParam(r, "pid"), rid)
	if err != nil {
		a.storeError(w, "get run", err)
		return
	}
	diffs := []store.DiffRow{}
	if limit > 0 {
		if diffs, err = a.store.RunDiffs(r.Context(), rid, limit); err != nil {
			a.storeError(w, "load diffs", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "diffs": diffs})
}

type claimRequest struct {
	WorkerID     string `json:"worker_id"`
	Variant      string `json:"variant"`
	LeaseSeconds int    `json:"lease_seconds"`
}

func (a *api) claimJob(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if !decodeBody(w, r, maxSmallBody, &req) {
		return
	}
	if !workerIDRe.MatchString(req.WorkerID) {
		writeError(w, http.StatusBadRequest, "worker_id must be 1 to 100 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	if !validVariant(req.Variant) {
		writeError(w, http.StatusBadRequest, "variant must be baseline_a, baseline_b or candidate")
		return
	}
	lease := defaultLease
	if req.LeaseSeconds != 0 {
		lease = time.Duration(req.LeaseSeconds) * time.Second
		if lease < minLease || lease > maxLease {
			writeError(w, http.StatusBadRequest, "lease_seconds must be between 30 and 3600")
			return
		}
	}
	job, err := a.store.ClaimReplayJob(r.Context(), chi.URLParam(r, "pid"), req.Variant, req.WorkerID, lease)
	if err != nil {
		a.storeError(w, "claim job", err)
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func jobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "jid"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}

type completeRequest struct {
	WorkerID string               `json:"worker_id"`
	Results  []store.ReplayResult `json:"results"`
}

func (a *api) completeJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	var req completeRequest
	if !decodeBody(w, r, maxCompleteBody, &req) {
		return
	}
	if !workerIDRe.MatchString(req.WorkerID) {
		writeError(w, http.StatusBadRequest, "worker_id must be 1 to 100 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	if err := a.store.CompleteReplayJob(r.Context(), chi.URLParam(r, "pid"), id, req.WorkerID, req.Results); err != nil {
		a.storeError(w, "complete job", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type failRequest struct {
	WorkerID string `json:"worker_id"`
	Error    string `json:"error"`
}

func (a *api) failJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	var req failRequest
	if !decodeBody(w, r, maxSmallBody, &req) {
		return
	}
	if !workerIDRe.MatchString(req.WorkerID) {
		writeError(w, http.StatusBadRequest, "worker_id must be 1 to 100 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	if req.Error == "" {
		req.Error = "worker reported failure"
	}
	retry, err := a.store.FailReplayJob(r.Context(), chi.URLParam(r, "pid"), id, req.WorkerID, req.Error)
	if err != nil {
		a.storeError(w, "fail job", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"will_retry": retry})
}
