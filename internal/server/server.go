// Package server is the REST API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Sangsty/replaygate/internal/auth"
	"github.com/Sangsty/replaygate/internal/model"
	"github.com/Sangsty/replaygate/internal/sanitize"
	"github.com/Sangsty/replaygate/internal/store"
)

const (
	// maxIngestBody bounds one ingest request. 500 requests of up to 64 KiB of
	// body each would exceed it, which is intended: GET bodies are rare.
	maxIngestBody = 8 << 20
	// maxIngestBatch is the most requests accepted in one ingest call.
	maxIngestBatch = 500
)

// Store is what the API needs from the database layer. *store.Store implements
// it; tests use a fake.
type Store interface {
	ProjectByAPIKey(ctx context.Context, plaintext string) (projectID string, err error)
	ProjectConfig(ctx context.Context, projectID string) (store.ProjectConfig, error)
	IngestRequests(ctx context.Context, projectID string, reqs []model.RecordedRequest) (int, error)

	CreateRun(ctx context.Context, p store.CreateRunParams) (store.Run, int, error)
	GetRun(ctx context.Context, projectID, runID string) (store.Run, error)
	RunDiffs(ctx context.Context, runID string, limit int) ([]store.DiffRow, error)
	ClaimReplayJob(ctx context.Context, projectID, variant, workerID string, lease time.Duration) (*store.ReplayJob, error)
	CompleteReplayJob(ctx context.Context, projectID string, jobID int64, workerID string, results []store.ReplayResult) error
	FailReplayJob(ctx context.Context, projectID string, jobID int64, workerID, msg string) (bool, error)

	// Dashboard: what signed-in users read.
	UpsertUser(ctx context.Context, githubID int64, login string) (userID string, err error)
	ProjectsForUser(ctx context.Context, userID, login string) ([]store.ProjectSummary, error)
	CanAccessProject(ctx context.Context, projectID, userID, login string) (bool, error)
	ListRuns(ctx context.Context, projectID string, limit int) ([]store.Run, error)
	RunNoise(ctx context.Context, runID string) ([]store.NoiseField, error)
	RunRequestDetail(ctx context.Context, projectID, runID string, requestID int64) (store.RequestDetail, error)
}

type api struct {
	store Store
	log   *slog.Logger
	auth  *AuthConfig // nil: dashboard sign-in is not configured
}

// Option customises the API.
type Option func(*api)

// New returns the API handler.
func New(st Store, log *slog.Logger, opts ...Option) http.Handler {
	a := &api{store: st, log: log}
	for _, o := range opts {
		o(a)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(accessLog(log))
	r.Use(middleware.Recoverer)

	r.Get("/healthz", a.healthz)

	// Dashboard sign-in. The routes exist only when sign-in is configured.
	if a.auth != nil {
		r.Get("/auth/github/login", a.githubLogin)
		r.Get("/auth/github/callback", a.githubCallback)
		r.Post("/auth/logout", a.logout)
	}
	r.Get("/v1/me", a.me)
	r.Get("/v1/projects", a.listProjects)

	r.Route("/v1/projects/{pid}", func(r chi.Router) {
		// Machines (CI, the recorder, replay workers) authenticate with the
		// project's API key, and only they may write.
		r.Group(func(r chi.Router) {
			r.Use(a.requireProjectKey)
			r.Get("/config", a.getConfig)
			r.Post("/ingest", a.ingest)

			r.Post("/runs", a.createRun)

			r.Post("/jobs/claim", a.claimJob)
			r.Post("/jobs/{jid}/complete", a.completeJob)
			r.Post("/jobs/{jid}/fail", a.failJob)
		})
		// Reading results works with the API key (the CLI polls with it) or
		// with a dashboard session of a project member.
		r.Group(func(r chi.Router) {
			r.Use(a.requireKeyOrSession)
			r.Get("/runs", a.listRuns)
			r.Get("/runs/{rid}", a.getRun)
			r.Get("/runs/{rid}/requests/{reqid}", a.getRunRequest)
		})
	})
	return r
}

func (a *api) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// requireProjectKey authenticates the caller with a per-project API key and
// checks that the key belongs to the project named in the URL.
func (a *api) requireProjectKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := auth.ParseBearer(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or malformed API key")
			return
		}
		projectID, err := a.store.ProjectByAPIKey(r.Context(), key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		case err != nil:
			a.log.Error("api key lookup failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if projectID != chi.URLParam(r, "pid") {
			writeError(w, http.StatusForbidden, "API key does not belong to this project")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *api) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.store.ProjectConfig(r.Context(), chi.URLParam(r, "pid"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		a.log.Error("load project config failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]json.RawMessage{
		"sanitize_rules": cfg.SanitizeRules,
		"diff_config":    cfg.DiffConfig,
	})
}

func (a *api) ingest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req model.IngestRequest
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body larger than %d bytes", maxIngestBody))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	switch n := len(req.Requests); {
	case n == 0:
		writeError(w, http.StatusBadRequest, "requests must not be empty")
		return
	case n > maxIngestBatch:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d requests per call, got %d", maxIngestBatch, n))
		return
	}
	for i := range req.Requests {
		if err := req.Requests[i].Validate(); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("requests[%d]: %v", i, err))
			return
		}
		// Defense in depth: even if a recorder forgot to redact, credentials
		// never reach the database.
		sanitize.Backstop(req.Requests[i].Headers)
	}

	accepted, err := a.store.IngestRequests(r.Context(), chi.URLParam(r, "pid"), req.Requests)
	if err != nil {
		a.log.Error("ingest failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, model.IngestResponse{Accepted: accepted})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// accessLog logs one structured line per request.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r)
			log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
			)
		})
	}
}
