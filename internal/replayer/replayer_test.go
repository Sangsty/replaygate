package replayer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sangsty/replaygate/internal/sanitize"
)

func target(t *testing.T, h http.Handler) *url.URL {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u
}

func TestDoSendsRecordedRequest(t *testing.T) {
	var got *http.Request
	var gotBody string
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	rp := &Replayer{Target: u, AuthHeader: "Bearer runner-token"}
	res := rp.Do(context.Background(), Request{
		ID: 7, Method: "GET", Path: "/users/42", Query: "a=1&b=two",
		Headers: map[string][]string{
			"accept":          {"application/json"},
			"authorization":   {sanitize.Redacted},
			"cookie":          {sanitize.Redacted},
			"accept-encoding": {"gzip"},
			"x-tenant":        {"acme", "beta"},
			"host":            {"prod.example.com"},
		},
		Body: []byte(`{"q":1}`),
	})

	if res.Error != "" || res.RequestID != 7 || res.Status != 201 || string(res.Body) != `{"ok":true}` {
		t.Fatalf("result = %+v", res)
	}
	if got.URL.Path != "/users/42" || got.URL.RawQuery != "a=1&b=two" || got.Method != "GET" {
		t.Errorf("target saw %s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Bearer runner-token" {
		t.Errorf("Authorization = %q, want the runner's token, never the redacted placeholder", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Cookie") != "" {
		t.Errorf("redacted cookie was sent: %q", got.Header.Get("Cookie"))
	}
	if got.Header.Get("Accept") != "application/json" || len(got.Header.Values("X-Tenant")) != 2 {
		t.Errorf("recorded headers not replayed: %v", got.Header)
	}
	if got.Host == "prod.example.com" {
		t.Error("recorded Host header overrode the target")
	}
	if gotBody != `{"q":1}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestDoWithoutAuthSendsNone(t *testing.T) {
	var auth string
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { auth = r.Header.Get("Authorization") }))
	(&Replayer{Target: u}).Do(context.Background(), Request{Method: "GET", Path: "/", Headers: map[string][]string{"authorization": {sanitize.Redacted}}})
	if auth != "" {
		t.Errorf("Authorization = %q, want none", auth)
	}
}

func TestDoDecompressesTransparently(t *testing.T) {
	var enc string
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc = r.Header.Get("Accept-Encoding")
		_, _ = io.WriteString(w, `plain`)
	}))
	res := (&Replayer{Target: u}).Do(context.Background(), Request{Method: "GET", Path: "/", Headers: map[string][]string{"accept-encoding": {"br"}}})
	if string(res.Body) != "plain" {
		t.Errorf("body = %q", res.Body)
	}
	if strings.Contains(enc, "br") {
		t.Errorf("recorded Accept-Encoding %q leaked; Go must negotiate it itself", enc)
	}
}

func TestDoJoinsTargetPathPrefix(t *testing.T) {
	var path string
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path }))
	u.Path = "/api/v2/"
	(&Replayer{Target: u}).Do(context.Background(), Request{Method: "GET", Path: "/users"})
	if path != "/api/v2/users" {
		t.Errorf("path = %q, want /api/v2/users", path)
	}
}

func TestDoDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
			return
		}
		_, _ = io.WriteString(w, "new")
	}))
	res := (&Replayer{Target: u}).Do(context.Background(), Request{Method: "GET", Path: "/old"})
	if res.Status != http.StatusMovedPermanently || hits.Load() != 1 {
		t.Errorf("status %d after %d hits; the redirect itself is the response under test", res.Status, hits.Load())
	}
}

func TestDoReportsNetworkFailuresAsResultErrors(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(dead.URL)
	dead.Close()
	res := (&Replayer{Target: u}).Do(context.Background(), Request{ID: 3, Method: "GET", Path: "/x", Query: "token=secret"})
	if res.Error == "" || res.Status != 0 || res.RequestID != 3 {
		t.Fatalf("result = %+v, want an error and no status", res)
	}
	if strings.Contains(res.Error, "secret") || strings.Contains(res.Error, "http://") {
		t.Errorf("error leaks the request URL: %q", res.Error)
	}
}

func TestDoTimeout(t *testing.T) {
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	res := (&Replayer{Target: u, Timeout: 50 * time.Millisecond}).Do(context.Background(), Request{Method: "GET", Path: "/"})
	if res.Error != "timeout" {
		t.Errorf("error = %q, want timeout", res.Error)
	}
}

func TestDoTruncatesLargeBodies(t *testing.T) {
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 1000))
	}))
	res := (&Replayer{Target: u, MaxBody: 100}).Do(context.Background(), Request{Method: "GET", Path: "/"})
	if !res.Truncated || len(res.Body) != 100 {
		t.Errorf("truncated=%v len=%d, want truncated at 100", res.Truncated, len(res.Body))
	}
	res = (&Replayer{Target: u, MaxBody: 1000}).Do(context.Background(), Request{Method: "GET", Path: "/"})
	if res.Truncated || len(res.Body) != 1000 {
		t.Errorf("a body exactly at the limit must not be flagged: truncated=%v len=%d", res.Truncated, len(res.Body))
	}
}

func TestDoRedactsResponseBodiesAtTheEdge(t *testing.T) {
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"email":"a@b.c","n":1}`)
	}))
	san, err := sanitize.New(sanitize.Rules{JSONPaths: []string{"$.email"}})
	if err != nil {
		t.Fatal(err)
	}
	res := (&Replayer{Target: u, Sanitizer: san}).Do(context.Background(), Request{Method: "GET", Path: "/"})
	if strings.Contains(string(res.Body), "a@b.c") || !strings.Contains(string(res.Body), sanitize.Redacted) {
		t.Errorf("body = %s; the email must be redacted before upload", res.Body)
	}
}

func TestBatchKeepsOrderAndBoundsConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32
	u := target(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	var reqs []Request
	for i := 0; i < 20; i++ {
		reqs = append(reqs, Request{ID: int64(i), Method: "GET", Path: "/r" + string(rune('a'+i))})
	}
	res := (&Replayer{Target: u, Concurrency: 3}).Batch(context.Background(), reqs)
	for i, r := range res {
		if r.RequestID != int64(i) || string(r.Body) != reqs[i].Path {
			t.Fatalf("result %d = %+v, want request %d", i, r, i)
		}
	}
	if peak.Load() > 3 {
		t.Errorf("peak concurrency = %d, want at most 3", peak.Load())
	}
}
