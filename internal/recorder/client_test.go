package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Sangsty/replaygate/internal/model"
)

func TestClientUpload(t *testing.T) {
	var gotAuth, gotCT, gotPath, gotMethod string
	var gotBody model.IngestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCT, gotPath, gotMethod = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"accepted":2}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "proj-1", "rg_secretkey")
	err := c.Upload(context.Background(), []model.RecordedRequest{rr("/a"), rr("/b")})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/projects/proj-1/ingest" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer rg_secretkey" || gotCT != "application/json" {
		t.Errorf("auth=%q content-type=%q", gotAuth, gotCT)
	}
	if len(gotBody.Requests) != 2 || gotBody.Requests[1].Path != "/b" {
		t.Errorf("server decoded %+v", gotBody)
	}
}

func TestClientUploadErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantErr       bool
		wantPermanent bool
	}{
		{"ok", http.StatusOK, false, false},
		{"accepted", http.StatusAccepted, false, false},
		{"bad request", http.StatusBadRequest, true, true},
		{"unauthorized", http.StatusUnauthorized, true, true},
		{"forbidden", http.StatusForbidden, true, true},
		{"too large", http.StatusRequestEntityTooLarge, true, true},
		{"request timeout is transient", http.StatusRequestTimeout, true, false},
		{"rate limited is transient", http.StatusTooManyRequests, true, false},
		{"server error is transient", http.StatusInternalServerError, true, false},
		{"bad gateway is transient", http.StatusBadGateway, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":"detail"}`))
			}))
			defer srv.Close()

			err := NewClient(srv.URL, "p", "rg_k").Upload(context.Background(), []model.RecordedRequest{rr("/a")})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && errors.Is(err, ErrPermanent) != tt.wantPermanent {
				t.Errorf("errors.Is(err, ErrPermanent) = %v, want %v (err: %v)", errors.Is(err, ErrPermanent), tt.wantPermanent, err)
			}
		})
	}
}

func TestClientUploadConnectionRefusedIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	err := NewClient(url, "p", "rg_k").Upload(context.Background(), []model.RecordedRequest{rr("/a")})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrPermanent) {
		t.Errorf("connection failure must be retried, got permanent: %v", err)
	}
}

func TestClientRules(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{
			"sanitize_rules": {"headers":["x-token"],"query_params":["api_key"],"json_paths":["$.email"]},
			"diff_config": {"fail_on":["breaking"]}
		}`))
	}))
	defer srv.Close()

	rules, err := NewClient(srv.URL, "proj-9", "rg_k").Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/projects/proj-9/config" || gotAuth != "Bearer rg_k" {
		t.Errorf("request path=%q auth=%q", gotPath, gotAuth)
	}
	if len(rules.Headers) != 1 || rules.Headers[0] != "x-token" ||
		len(rules.QueryParams) != 1 || rules.QueryParams[0] != "api_key" ||
		len(rules.JSONPaths) != 1 || rules.JSONPaths[0] != "$.email" {
		t.Errorf("rules = %+v", rules)
	}
}

func TestClientRulesErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":"x"}`, true},
		{"server error", http.StatusInternalServerError, ``, true},
		{"malformed JSON", http.StatusOK, `not json`, true},
		{"missing rules decode to empty", http.StatusOK, `{}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, "p", "rg_k").Rules(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
