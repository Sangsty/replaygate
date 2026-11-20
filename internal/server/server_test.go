package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Sangsty/replaygate/internal/model"
	"github.com/Sangsty/replaygate/internal/store"
)

const (
	projA  = "11111111-1111-1111-1111-111111111111"
	projB  = "22222222-2222-2222-2222-222222222222"
	keyA   = "rg_keyforprojecta"
	keyB   = "rg_keyforprojectb"
	cfgURL = "/v1/projects/" + projA + "/config"
	inURL  = "/v1/projects/" + projA + "/ingest"
)

type fakeStore struct {
	mu        sync.Mutex
	keys      map[string]string
	cfg       store.ProjectConfig
	cfgErr    error
	ingestErr error
	lookupErr error
	ingested  [][]model.RecordedRequest
}

func newFake() *fakeStore {
	return &fakeStore{
		keys: map[string]string{keyA: projA, keyB: projB},
		cfg: store.ProjectConfig{
			SanitizeRules: json.RawMessage(`{"headers":["x-secret"],"query_params":[],"json_paths":[]}`),
			DiffConfig:    json.RawMessage(`{"fail_on":["breaking"]}`),
		},
	}
}

func (f *fakeStore) ProjectByAPIKey(_ context.Context, key string) (string, error) {
	if f.lookupErr != nil {
		return "", f.lookupErr
	}
	if p, ok := f.keys[key]; ok {
		return p, nil
	}
	return "", store.ErrNotFound
}

func (f *fakeStore) ProjectConfig(context.Context, string) (store.ProjectConfig, error) {
	return f.cfg, f.cfgErr
}

func (f *fakeStore) IngestRequests(_ context.Context, _ string, reqs []model.RecordedRequest) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ingestErr != nil {
		return 0, f.ingestErr
	}
	f.ingested = append(f.ingested, reqs)
	return len(reqs), nil
}

func newHandler(f *fakeStore) http.Handler {
	return New(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(h http.Handler, method, url, authz string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func validIngest() string {
	return `{"requests":[
		{"method":"GET","path":"/users/1","query":"a=1","headers":{"accept":["application/json"],"Authorization":["Bearer leaked"],"Cookie":["s=1"]},"status":200,"latency_ms":4},
		{"method":"GET","path":"/users/2","status":404,"latency_ms":2}
	]}`
}

func TestHealthz(t *testing.T) {
	rec := do(newHandler(newFake()), http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body)
	}
}

func TestAuth(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		url        string
		authz      string
		body       string
		lookupErr  error
		wantStatus int
	}{
		{"config without header", http.MethodGet, cfgURL, "", "", nil, http.StatusUnauthorized},
		{"config wrong scheme", http.MethodGet, cfgURL, "Basic " + keyA, "", nil, http.StatusUnauthorized},
		{"config key without prefix", http.MethodGet, cfgURL, "Bearer nokeyprefix", "", nil, http.StatusUnauthorized},
		{"config unknown key", http.MethodGet, cfgURL, "Bearer rg_unknown", "", nil, http.StatusUnauthorized},
		{"config other project's key", http.MethodGet, cfgURL, "Bearer " + keyB, "", nil, http.StatusForbidden},
		{"config store failure", http.MethodGet, cfgURL, "Bearer " + keyA, "", errors.New("db down"), http.StatusInternalServerError},
		{"config ok", http.MethodGet, cfgURL, "Bearer " + keyA, "", nil, http.StatusOK},
		{"ingest without header", http.MethodPost, inURL, "", validIngest(), nil, http.StatusUnauthorized},
		{"ingest other project's key", http.MethodPost, inURL, "Bearer " + keyB, validIngest(), nil, http.StatusForbidden},
		{"ingest malformed project id", http.MethodPost, "/v1/projects/not-a-uuid/ingest", "Bearer " + keyA, validIngest(), nil, http.StatusForbidden},
		{"ingest ok", http.MethodPost, inURL, "Bearer " + keyA, validIngest(), nil, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.lookupErr = tt.lookupErr
			rec := do(newHandler(f), tt.method, tt.url, tt.authz, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate header")
			}
			if tt.wantStatus != http.StatusOK && len(f.ingested) != 0 {
				t.Error("store was called for an unauthenticated request")
			}
		})
	}
}

func TestGetConfig(t *testing.T) {
	rec := do(newHandler(newFake()), http.MethodGet, cfgURL, "Bearer "+keyA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		SanitizeRules struct {
			Headers []string `json:"headers"`
		} `json:"sanitize_rules"`
		DiffConfig map[string]any `json:"diff_config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.SanitizeRules.Headers) != 1 || got.SanitizeRules.Headers[0] != "x-secret" {
		t.Errorf("sanitize_rules.headers = %v", got.SanitizeRules.Headers)
	}
	if got.DiffConfig["fail_on"] == nil {
		t.Errorf("diff_config missing: %s", rec.Body)
	}
}

func TestGetConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"project missing", store.ErrNotFound, http.StatusNotFound},
		{"database error", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.cfgErr = tt.err
			rec := do(newHandler(f), http.MethodGet, cfgURL, "Bearer "+keyA, "")
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestIngestSuccessStripsCredentials(t *testing.T) {
	f := newFake()
	rec := do(newHandler(f), http.MethodPost, inURL, "Bearer "+keyA, validIngest())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	var resp model.IngestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Accepted != 2 {
		t.Fatalf("response = %s (err %v), want accepted=2", rec.Body, err)
	}
	if len(f.ingested) != 1 || len(f.ingested[0]) != 2 {
		t.Fatalf("store received %v", f.ingested)
	}
	h := f.ingested[0][0].Headers
	for name := range h {
		if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "cookie") {
			t.Errorf("credential header %q reached the store", name)
		}
	}
	if h["accept"] == nil {
		t.Error("harmless header was dropped")
	}
}

func TestIngestRejectsBadInput(t *testing.T) {
	many := `{"requests":[` + strings.TrimSuffix(strings.Repeat(`{"method":"GET","path":"/x","status":200},`, maxIngestBatch+1), ",") + `]}`
	huge := `{"requests":[{"method":"GET","path":"/` + strings.Repeat("a", maxIngestBody) + `","status":200}]}`

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantSubstr string
	}{
		{"not json", `nope`, http.StatusBadRequest, "invalid JSON"},
		{"unknown field", `{"requests":[],"extra":1}`, http.StatusBadRequest, "invalid JSON"},
		{"empty batch", `{"requests":[]}`, http.StatusBadRequest, "must not be empty"},
		{"missing requests", `{}`, http.StatusBadRequest, "must not be empty"},
		{"post method", `{"requests":[{"method":"POST","path":"/x","status":200}]}`, http.StatusBadRequest, "requests[0]"},
		{"relative path", `{"requests":[{"method":"GET","path":"x","status":200}]}`, http.StatusBadRequest, "path must start"},
		{"status out of range", `{"requests":[{"method":"GET","path":"/ok","status":200},{"method":"GET","path":"/x","status":700}]}`, http.StatusBadRequest, "requests[1]"},
		{"too many requests", many, http.StatusBadRequest, "at most"},
		{"body too large", huge, http.StatusRequestEntityTooLarge, "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			rec := do(newHandler(f), http.MethodPost, inURL, "Bearer "+keyA, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %.200s", rec.Code, tt.wantStatus, rec.Body)
			}
			if !bytes.Contains(rec.Body.Bytes(), []byte(tt.wantSubstr)) {
				t.Errorf("body %.200s does not contain %q", rec.Body, tt.wantSubstr)
			}
			if len(f.ingested) != 0 {
				t.Error("invalid batch reached the store")
			}
		})
	}
}

func TestIngestStoreFailure(t *testing.T) {
	f := newFake()
	f.ingestErr = errors.New("db down")
	rec := do(newHandler(f), http.MethodPost, inURL, "Bearer "+keyA, validIngest())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "db down") {
		t.Error("internal error detail leaked to the client")
	}
}
