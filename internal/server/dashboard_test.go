package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sangsty/replaygate/internal/github"
	"github.com/Sangsty/replaygate/internal/report"
	"github.com/Sangsty/replaygate/internal/session"
	"github.com/Sangsty/replaygate/internal/store"
)

const (
	userAlice = "aaaaaaaa-0000-0000-0000-000000000001"
	userEve   = "eeeeeeee-0000-0000-0000-000000000009"
	signSec   = "0123456789abcdef0123456789abcdef-test-secret"
	dashURL   = "https://rg.example/"
)

// dashFake implements the dashboard half of the Store interface.
type dashFake struct {
	members    map[string]bool // userID+"|"+projectID
	projects   []store.ProjectSummary
	runs       []store.Run
	runsLimit  int
	noise      []store.NoiseField
	detail     store.RequestDetail
	detailErr  error
	accessErr  error
	upserted   [2]string
	upsertErr  error
	upsertedID int64
}

func (f *dashFake) UpsertUser(_ context.Context, ghID int64, login string) (string, error) {
	f.upsertedID, f.upserted = ghID, [2]string{login, ""}
	return userAlice, f.upsertErr
}
func (f *dashFake) ProjectsForUser(_ context.Context, uid, _ string) ([]store.ProjectSummary, error) {
	if uid != userAlice {
		return []store.ProjectSummary{}, nil
	}
	return f.projects, nil
}
func (f *dashFake) CanAccessProject(_ context.Context, pid, uid, _ string) (bool, error) {
	return f.members[uid+"|"+pid], f.accessErr
}
func (f *dashFake) ListRuns(_ context.Context, _ string, limit int) ([]store.Run, error) {
	f.runsLimit = limit
	return f.runs, nil
}
func (f *dashFake) RunNoise(context.Context, string) ([]store.NoiseField, error) {
	if f.noise == nil {
		return []store.NoiseField{}, nil
	}
	return f.noise, nil
}
func (f *dashFake) RunRequestDetail(context.Context, string, string, int64) (store.RequestDetail, error) {
	return f.detail, f.detailErr
}

type authEnv struct {
	f       *fakeStore
	h       http.Handler
	mgr     *session.Manager
	gh      *httptest.Server
	ghUsers int
}

// newAuthEnv builds a handler with sign-in enabled against a fake GitHub.
func newAuthEnv(t *testing.T, secure bool) *authEnv {
	t.Helper()
	e := &authEnv{f: newFake()}
	e.f.members = map[string]bool{userAlice + "|" + projA: true}
	e.f.projects = []store.ProjectSummary{{ID: projA, Name: "demo", CreatedAt: time.Unix(1, 0).UTC()}}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("code") != "good-code" {
			_, _ = io.WriteString(w, `{"error":"bad_verification_code"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"tok"}`)
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		e.ghUsers++
		_, _ = io.WriteString(w, `{"id":4242,"login":"Alice","avatar_url":""}`)
	})
	e.gh = httptest.NewServer(mux)
	t.Cleanup(e.gh.Close)

	var err error
	if e.mgr, err = session.NewManager(signSec, time.Hour); err != nil {
		t.Fatal(err)
	}
	e.h = New(e.f, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAuth(AuthConfig{
		Sessions: e.mgr, DashboardURL: dashURL, SecureCookies: secure,
		OAuth: &github.OAuth{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://rg.example/auth/github/callback",
			AuthorizeURL: e.gh.URL + "/authorize", TokenURL: e.gh.URL + "/token", APIURL: e.gh.URL},
	}))
	return e
}

func (e *authEnv) cookie(t *testing.T, userID, login string) *http.Cookie {
	t.Helper()
	tok, err := e.mgr.Issue(userID, 4242, login)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

func (e *authEnv) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return e.req(http.MethodGet, path, "", cookies...)
}

func (e *authEnv) req(method, path, authz string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestSessionGrantsReadAccessToMembersOnly(t *testing.T) {
	e := newAuthEnv(t, false)
	e.f.run = store.Run{ID: runUUID, Status: "failed"}
	runURL := "/v1/projects/" + projA + "/runs/" + runUUID

	tests := []struct {
		name string
		rec  *httptest.ResponseRecorder
		want int
	}{
		{"member reads a run", e.get(runURL, e.cookie(t, userAlice, "alice")), http.StatusOK},
		{"member lists runs", e.get("/v1/projects/"+projA+"/runs", e.cookie(t, userAlice, "alice")), http.StatusOK},
		{"non-member gets 404, not 403", e.get(runURL, e.cookie(t, userEve, "eve")), http.StatusNotFound},
		{"no credentials", e.get(runURL), http.StatusUnauthorized},
		{"forged cookie", e.get(runURL, &http.Cookie{Name: sessionCookie, Value: "a.b.c"}), http.StatusUnauthorized},
		{"cookie signed with another secret", func() *httptest.ResponseRecorder {
			other, _ := session.NewManager("another-secret-another-secret-another!", time.Hour)
			tok, _ := other.Issue(userAlice, 1, "alice")
			return e.get(runURL, &http.Cookie{Name: sessionCookie, Value: tok})
		}(), http.StatusUnauthorized},
		{"api key still works", e.req(http.MethodGet, runURL, bearerA()), http.StatusOK},
		{"another project's key is still refused", e.req(http.MethodGet, runURL, "Bearer "+keyB), http.StatusForbidden},
		{"a bad api key is not rescued by a valid session", e.req(http.MethodGet, runURL, "Bearer rg_nope", e.cookie(t, userAlice, "alice")), http.StatusUnauthorized},
		{"malformed project id", e.get("/v1/projects/not-a-uuid/runs", e.cookie(t, userAlice, "alice")), http.StatusNotFound},
	}
	for _, tt := range tests {
		if tt.rec.Code != tt.want {
			t.Errorf("%s: status = %d, want %d (%s)", tt.name, tt.rec.Code, tt.want, tt.rec.Body)
		}
	}
	if cc := e.get(runURL, e.cookie(t, userAlice, "alice")).Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; private data must not be cached", cc)
	}
}

// A session cookie is sent by the browser on cross-site requests, so it must
// never be enough to change anything.
func TestSessionCannotWrite(t *testing.T) {
	e := newAuthEnv(t, false)
	c := e.cookie(t, userAlice, "alice")
	base := "/v1/projects/" + projA
	for _, ep := range []struct{ method, path string }{
		{"POST", base + "/runs"}, {"POST", base + "/ingest"}, {"POST", base + "/jobs/claim"},
		{"POST", base + "/jobs/1/complete"}, {"POST", base + "/jobs/1/fail"}, {"GET", base + "/config"},
	} {
		if rec := e.req(ep.method, ep.path, "", c); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with only a session = %d, want 401", ep.method, ep.path, rec.Code)
		}
	}
}

// The read middleware refuses sessions on anything but GET/HEAD, so a write
// route added behind it by mistake is still safe from cross-site forgery.
func TestReadMiddlewareRefusesSessionWrites(t *testing.T) {
	e := newAuthEnv(t, false)
	a := &api{store: e.f, log: slog.New(slog.NewTextHandler(io.Discard, nil)), auth: &AuthConfig{Sessions: e.mgr}}
	r := chi.NewRouter()
	reached := false
	r.With(a.requireKeyOrSession).HandleFunc("/v1/projects/{pid}/x", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		req := httptest.NewRequest(m, "/v1/projects/"+projA+"/x", nil)
		req.AddCookie(e.cookie(t, userAlice, "alice"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed || reached {
			t.Errorf("%s with a session = %d (handler reached: %v)", m, rec.Code, reached)
		}
	}
	req := httptest.NewRequest("GET", "/v1/projects/"+projA+"/x", nil)
	req.AddCookie(e.cookie(t, userAlice, "alice"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !reached {
		t.Errorf("GET with a session = %d", rec.Code)
	}
}

func TestMeAndProjects(t *testing.T) {
	e := newAuthEnv(t, false)
	rec := e.get("/v1/me", e.cookie(t, userAlice, "alice"))
	var me map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &me) != nil || me["login"] != "alice" || me["user_id"] != userAlice {
		t.Errorf("me = %d %s", rec.Code, rec.Body)
	}
	rec = e.get("/v1/projects", e.cookie(t, userAlice, "alice"))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"demo"`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("projects = %d %s", rec.Code, rec.Body)
	}
	if rec := e.get("/v1/projects", e.cookie(t, userEve, "eve")); !strings.Contains(rec.Body.String(), `"projects":[]`) {
		t.Errorf("a user with no projects must get [], got %s", rec.Body)
	}
	for _, p := range []string{"/v1/me", "/v1/projects"} {
		if rec := e.get(p); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s signed out = %d", p, rec.Code)
		}
	}
}

func TestSignInDisabledWithoutConfig(t *testing.T) {
	h := newHandler(newFake())
	for _, p := range []string{"/auth/github/login", "/auth/github/callback"} {
		if rec := do(h, "GET", p, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 when sign-in is not configured", p, rec.Code)
		}
	}
	if rec := do(h, "GET", "/v1/me", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1/me = %d", rec.Code)
	}
	// Reads with an API key work as before.
	f := newFake()
	f.run = store.Run{ID: runUUID, Status: "passed"}
	if rec := do(newHandler(f), "GET", runsURL+"/"+runUUID, bearerA(), ""); rec.Code != 200 {
		t.Errorf("key read = %d", rec.Code)
	}
}

func TestLoginRedirectsToGitHubWithState(t *testing.T) {
	e := newAuthEnv(t, true)
	rec := e.get("/auth/github/login")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	if !strings.HasPrefix(loc.String(), e.gh.URL+"/authorize") || loc.Query().Get("client_id") != "cid" || len(state) < 20 {
		t.Errorf("location = %s", loc)
	}
	c := findCookie(rec, stateCookie)
	if c == nil || c.Value != state || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/auth" {
		t.Errorf("state cookie = %+v", c)
	}
	if other := findCookie(e.get("/auth/github/login"), stateCookie); other == nil || other.Value == state {
		t.Error("state must be fresh for every login")
	}
}

func callback(e *authEnv, state, cookieState, code string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/auth/github/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
	if cookieState != "" {
		req.AddCookie(&http.Cookie{Name: stateCookie, Value: cookieState})
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestCallbackSignsTheUserIn(t *testing.T) {
	e := newAuthEnv(t, true)
	rec := callback(e, "st4te", "st4te", "good-code")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != dashURL {
		t.Fatalf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
	c := findCookie(rec, sessionCookie)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 3600 {
		t.Fatalf("session cookie = %+v", c)
	}
	claims, err := e.mgr.Verify(c.Value)
	if err != nil || claims.UserID != userAlice || claims.Login != "Alice" || claims.GitHubID != 4242 {
		t.Errorf("claims = %+v, %v", claims, err)
	}
	if e.f.upsertedID != 4242 || e.f.upserted[0] != "Alice" {
		t.Errorf("user not stored: %d %v", e.f.upsertedID, e.f.upserted)
	}
	if cleared := findCookie(rec, stateCookie); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("state cookie must be cleared after use: %+v", cleared)
	}
	// The session it issued opens the member's project.
	if rec := e.get("/v1/projects/"+projA+"/runs", c); rec.Code != 200 {
		t.Errorf("new session cannot read: %d", rec.Code)
	}
}

func TestCallbackRejectsBadAttempts(t *testing.T) {
	tests := []struct {
		name string
		do   func(e *authEnv) *httptest.ResponseRecorder
		code string
	}{
		{"state mismatch", func(e *authEnv) *httptest.ResponseRecorder { return callback(e, "attacker", "victim", "good-code") }, "state"},
		{"no state cookie", func(e *authEnv) *httptest.ResponseRecorder { return callback(e, "st", "", "good-code") }, "state"},
		{"no state in the URL", func(e *authEnv) *httptest.ResponseRecorder { return callback(e, "", "st", "good-code") }, "state"},
		{"github rejects the code", func(e *authEnv) *httptest.ResponseRecorder { return callback(e, "st", "st", "stale-code") }, "failed"},
		{"user cancelled", func(e *authEnv) *httptest.ResponseRecorder {
			req := httptest.NewRequest("GET", "/auth/github/callback?state=st&error=access_denied", nil)
			req.AddCookie(&http.Cookie{Name: stateCookie, Value: "st"})
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			return rec
		}, "denied"},
		{"storing the user fails", func(e *authEnv) *httptest.ResponseRecorder {
			e.f.upsertErr = errors.New("db down")
			return callback(e, "st", "st", "good-code")
		}, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAuthEnv(t, false)
			rec := tt.do(e)
			loc, _ := url.Parse(rec.Header().Get("Location"))
			if rec.Code != http.StatusFound || loc.Query().Get("login_error") != tt.code || loc.Host != "rg.example" {
				t.Errorf("status = %d, location = %s, want login_error=%s", rec.Code, loc, tt.code)
			}
			if findCookie(rec, sessionCookie) != nil {
				t.Error("a session was issued for a failed sign-in")
			}
			if tt.code == "state" && e.ghUsers != 0 {
				t.Error("GitHub was called before the state was verified")
			}
		})
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	e := newAuthEnv(t, false)
	rec := e.req(http.MethodPost, "/auth/logout", "", e.cookie(t, userAlice, "alice"))
	c := findCookie(rec, sessionCookie)
	if rec.Code != http.StatusNoContent || c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("logout = %d %+v", rec.Code, c)
	}
}

func TestListRunsParameters(t *testing.T) {
	e := newAuthEnv(t, false)
	c := e.cookie(t, userAlice, "alice")
	e.f.runs = []store.Run{{ID: runUUID, CommitSHA: "abc1234", Status: "passed"}}
	base := "/v1/projects/" + projA + "/runs"

	rec := e.get(base, c)
	if rec.Code != 200 || e.f.runsLimit != 50 || !strings.Contains(rec.Body.String(), runUUID) {
		t.Errorf("default = %d limit=%d %s", rec.Code, e.f.runsLimit, rec.Body)
	}
	if rec := e.get(base+"?limit=7", c); rec.Code != 200 || e.f.runsLimit != 7 {
		t.Errorf("limit=7 → %d, store got %d", rec.Code, e.f.runsLimit)
	}
	for _, bad := range []string{"0", "101", "-1", "x"} {
		if rec := e.get(base+"?limit="+bad, c); rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%s = %d, want 400", bad, rec.Code)
		}
	}
}

func TestGetRunRequest(t *testing.T) {
	e := newAuthEnv(t, false)
	c := e.cookie(t, userAlice, "alice")
	e.f.detail = store.RequestDetail{
		Request:   store.RequestInfo{ID: 5, Method: "GET", Path: "/users/3", Endpoint: "/users/{id}"},
		Baseline:  &store.ResponseDetail{Status: 200, Body: `{"name":"x"}`},
		Candidate: &store.ResponseDetail{Status: 200, Body: `{}`},
		Diffs:     []store.DiffRow{},
	}
	base := "/v1/projects/" + projA + "/runs/" + runUUID + "/requests/"
	rec := e.get(base+"5", c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"endpoint":"/users/{id}"`) || !strings.Contains(rec.Body.String(), `"candidate":{`) {
		t.Errorf("detail = %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"0", "-3", "abc", "99999999999999999999"} {
		if rec := e.get(base+bad, c); rec.Code != http.StatusNotFound {
			t.Errorf("request id %q = %d, want 404", bad, rec.Code)
		}
	}
	if rec := e.get("/v1/projects/"+projA+"/runs/not-a-uuid/requests/5", c); rec.Code != http.StatusNotFound {
		t.Errorf("bad run id = %d", rec.Code)
	}
	e.f.detailErr = store.ErrNotFound
	if rec := e.get(base+"5", c); rec.Code != http.StatusNotFound {
		t.Errorf("missing request = %d", rec.Code)
	}
}

// The CLI decodes the run response into report.Result. If the API's JSON and
// that struct drift apart the PR comment silently loses data, so pin them.
func TestRunResponseMatchesTheReportContract(t *testing.T) {
	f := newFake()
	pr := 7
	now := time.Now().UTC()
	f.run = store.Run{ID: runUUID, CommitSHA: "abc1234", PRNumber: &pr, Status: "failed", TotalRequests: 3, Breaking: 1,
		Error: "x", CreatedAt: now, FinishedAt: &now}
	f.diffs = []store.DiffRow{{RequestID: 1, Endpoint: "/u/{id}", Severity: "breaking", Kind: "value_changed", JSONPath: "$.a",
		Baseline: json.RawMessage(`1`), Candidate: json.RawMessage(`2`)}}
	f.noise = []store.NoiseField{{Endpoint: "/u/{id}", JSONPath: "$.ts"}}
	rec := do(newHandler(f), "GET", runsURL+"/"+runUUID, bearerA(), "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var res report.Result
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		t.Fatalf("API response no longer decodes into report.Result: %v", err)
	}
	if res.Run.PRNumber == nil || *res.Run.PRNumber != 7 || res.Run.FinishedAt == nil || len(res.Diffs) != 1 ||
		string(res.Diffs[0].Candidate) != "2" || len(res.Noise) != 1 || res.Noise[0].JSONPath != "$.ts" {
		t.Errorf("decoded = %+v", res)
	}
}
