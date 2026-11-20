package recorder

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sangsty/replaygate/internal/model"
	"github.com/Sangsty/replaygate/internal/sanitize"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeSink struct {
	mu   sync.Mutex
	reqs []model.RecordedRequest
}

func (s *fakeSink) Submit(r model.RecordedRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
}

func (s *fakeSink) all() []model.RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.RecordedRequest(nil), s.reqs...)
}

// newTestProxy starts an upstream and a proxy in front of it. rules == nil
// leaves the proxy without sanitizer rules (it must record nothing then).
func newTestProxy(t *testing.T, upstream http.Handler, rules *sanitize.Rules, maxBody int64) (*httptest.Server, *fakeSink) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	return newProxyFor(t, up.URL, rules, maxBody)
}

func newProxyFor(t *testing.T, upstreamURL string, rules *sanitize.Rules, maxBody int64) (*httptest.Server, *fakeSink) {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	p := NewProxy(Options{Upstream: u, Sink: sink, Logger: discard, MaxBodyBytes: maxBody})
	if rules != nil {
		if err := p.SetRules(*rules); err != nil {
			t.Fatal(err)
		}
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front, sink
}

func get(t *testing.T, rawURL string, headers map[string]string, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestProxyRecordsSanitizedGET(t *testing.T) {
	var gotAuth, gotCookie string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("X-Upstream", "1")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello")
	})
	front, sink := newTestProxy(t, upstream, &sanitize.Rules{QueryParams: []string{"token"}}, 0)

	resp := get(t, front.URL+"/users/42?token=abc&a=1", map[string]string{
		"Authorization": "Bearer secret",
		"Cookie":        "session=xyz",
		"Accept":        "application/json",
	}, "")
	body, _ := io.ReadAll(resp.Body)

	// The client must see the upstream response untouched.
	if resp.StatusCode != http.StatusCreated || string(body) != "hello" || resp.Header.Get("X-Upstream") != "1" {
		t.Fatalf("client saw %d %q %q, want 201 hello 1", resp.StatusCode, body, resp.Header.Get("X-Upstream"))
	}
	// The upstream must see the original, unredacted credentials.
	if gotAuth != "Bearer secret" || gotCookie != "session=xyz" {
		t.Errorf("upstream saw Authorization=%q Cookie=%q; sanitizing must not alter forwarded traffic", gotAuth, gotCookie)
	}

	recs := sink.all()
	if len(recs) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(recs))
	}
	r := recs[0]
	if r.Method != "GET" || r.Path != "/users/42" || r.Status != http.StatusCreated {
		t.Errorf("recorded %s %s %d", r.Method, r.Path, r.Status)
	}
	if r.Query != "token=%5BREDACTED%5D&a=1" {
		t.Errorf("query = %q, want token redacted and order kept", r.Query)
	}
	if got := r.Headers["authorization"]; len(got) != 1 || got[0] != sanitize.Redacted {
		t.Errorf("authorization = %v, want redacted", got)
	}
	if got := r.Headers["cookie"]; len(got) != 1 || got[0] != sanitize.Redacted {
		t.Errorf("cookie = %v, want redacted", got)
	}
	if got := r.Headers["accept"]; len(got) != 1 || got[0] != "application/json" {
		t.Errorf("accept = %v, want preserved", got)
	}
	if _, ok := r.Headers["accept-encoding"]; ok {
		t.Error("accept-encoding should not be recorded")
	}
	if r.LatencyMS < 0 {
		t.Errorf("latency = %d", r.LatencyMS)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("recorded request fails API validation: %v", err)
	}
}

func TestProxyRecordsErrorStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"not found", http.StatusNotFound},
		{"server error", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			front, sink := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}), &sanitize.Rules{}, 0)
			if got := get(t, front.URL+"/x", nil, "").StatusCode; got != tt.status {
				t.Fatalf("client status = %d, want %d", got, tt.status)
			}
			recs := sink.all()
			if len(recs) != 1 || recs[0].Status != tt.status {
				t.Fatalf("recorded %+v, want one request with status %d", recs, tt.status)
			}
		})
	}
}

func TestProxyDefaultsToStatus200(t *testing.T) {
	front, sink := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "no explicit WriteHeader")
	}), &sanitize.Rules{}, 0)
	get(t, front.URL+"/x", nil, "")
	if recs := sink.all(); len(recs) != 1 || recs[0].Status != 200 {
		t.Fatalf("recorded %+v, want status 200", recs)
	}
}

func TestProxyIgnoresNonGET(t *testing.T) {
	var gotMethod string
	front, sink := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}), &sanitize.Rules{}, 0)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		req, _ := http.NewRequest(method, front.URL+"/x", strings.NewReader("data"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if gotMethod != method {
			t.Errorf("upstream saw %s, want %s", gotMethod, method)
		}
	}
	if n := len(sink.all()); n != 0 {
		t.Errorf("recorded %d non-GET requests, want 0", n)
	}
}

func TestProxyUpstreamDown(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // nothing listens there any more

	front, sink := newProxyFor(t, deadURL, &sanitize.Rules{}, 0)
	if got := get(t, front.URL+"/x", nil, "").StatusCode; got != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", got)
	}
	if n := len(sink.all()); n != 0 {
		t.Errorf("recorded %d requests for a proxy-level failure, want 0", n)
	}
}

func TestProxyWithoutRulesRecordsNothing(t *testing.T) {
	front, sink := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}), nil, 0)
	resp := get(t, front.URL+"/x", map[string]string{"Authorization": "Bearer secret"}, "")
	if body, _ := io.ReadAll(resp.Body); string(body) != "ok" {
		t.Fatalf("traffic not forwarded: %q", body)
	}
	if n := len(sink.all()); n != 0 {
		t.Errorf("recorded %d requests before rules were loaded, want 0 (fail closed)", n)
	}
}

func TestProxyBodyHandling(t *testing.T) {
	const payload = `{"email":"a@b.c","q":1}`
	tests := []struct {
		name         string
		rules        sanitize.Rules
		maxBody      int64
		body         string
		wantRecorded bool
		wantBody     string
	}{
		{
			name:         "body redacted by JSONPath",
			rules:        sanitize.Rules{JSONPaths: []string{"$.email"}},
			body:         payload,
			wantRecorded: true,
			wantBody:     `{"email":"[REDACTED]","q":1}`,
		},
		{
			name:         "body kept when no JSONPath rules",
			rules:        sanitize.Rules{},
			body:         payload,
			wantRecorded: true,
			wantBody:     payload,
		},
		{
			name:         "non-JSON body dropped but request recorded",
			rules:        sanitize.Rules{JSONPaths: []string{"$.email"}},
			body:         "plain text",
			wantRecorded: true,
			wantBody:     "",
		},
		{
			name:         "oversized body not recorded",
			rules:        sanitize.Rules{},
			maxBody:      8,
			body:         payload,
			wantRecorded: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamBody string
			front, sink := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				upstreamBody = string(b)
				w.WriteHeader(http.StatusOK)
			}), &tt.rules, tt.maxBody)

			get(t, front.URL+"/search", nil, tt.body)

			if upstreamBody != tt.body {
				t.Errorf("upstream received %q, want the full original %q", upstreamBody, tt.body)
			}
			recs := sink.all()
			if tt.wantRecorded != (len(recs) == 1) {
				t.Fatalf("recorded %d requests, wantRecorded=%v", len(recs), tt.wantRecorded)
			}
			if tt.wantRecorded && string(recs[0].Body) != tt.wantBody {
				t.Errorf("recorded body = %q, want %q", recs[0].Body, tt.wantBody)
			}
		})
	}
}

func TestProxyDoesNotRecordUpgradeRequests(t *testing.T) {
	front, sink := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), &sanitize.Rules{}, 0)
	get(t, front.URL+"/ws", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}, "")
	if n := len(sink.all()); n != 0 {
		t.Errorf("recorded %d upgrade requests, want 0", n)
	}
}

func TestSetRulesKeepsPreviousOnError(t *testing.T) {
	u, _ := url.Parse("http://example.invalid")
	p := NewProxy(Options{Upstream: u, Sink: &fakeSink{}, Logger: discard})
	if err := p.SetRules(sanitize.Rules{JSONPaths: []string{"$.a[1:2]"}}); err == nil {
		t.Fatal("expected error for invalid JSONPath")
	}
	if p.san.Load() != nil {
		t.Error("invalid rules were installed")
	}
	if err := p.SetRules(sanitize.Rules{}); err != nil {
		t.Fatal(err)
	}
	before := p.san.Load()
	if err := p.SetRules(sanitize.Rules{JSONPaths: []string{"bad"}}); err == nil {
		t.Fatal("expected error")
	}
	if p.san.Load() != before {
		t.Error("a failed SetRules replaced the previous good rules")
	}
}

func TestRefreshRules(t *testing.T) {
	u, _ := url.Parse("http://example.invalid")
	p := NewProxy(Options{Upstream: u, Sink: &fakeSink{}, Logger: discard})

	var mu sync.Mutex
	calls := 0
	fetch := func(context.Context) (sanitize.Rules, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls < 3 {
			return sanitize.Rules{}, errors.New("api not ready")
		}
		return sanitize.Rules{QueryParams: []string{"token"}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RefreshRules(ctx, fetch, time.Hour, 5*time.Millisecond)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for p.san.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("rules were never loaded after transient failures")
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	if calls != 3 {
		t.Errorf("fetch called %d times, want 3 (two failures, then success)", calls)
	}
	mu.Unlock()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshRules did not stop after context cancel")
	}
}
