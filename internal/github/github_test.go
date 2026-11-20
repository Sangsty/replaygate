package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const marker = "<!-- replaygate:report -->"

// fakeGitHub keeps one PR's comments in memory.
type fakeGitHub struct {
	mu       sync.Mutex
	comments []comment
	next     int64
	patch403 bool
	calls    []string
	authz    string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.authz = r.Header.Get("Authorization")
	body, _ := io.ReadAll(r.Body)
	var in struct{ Body string }
	_ = json.Unmarshal(body, &in)

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
		page := r.URL.Query().Get("page")
		out := []comment{}
		if page == "1" || page == "" {
			out = f.comments
			if len(out) > perPage {
				out = out[:perPage]
			}
		} else if page == "2" && len(f.comments) > perPage {
			out = f.comments[perPage:]
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "POST":
		f.next++
		c := comment{ID: f.next, Body: in.Body, HTMLURL: fmt.Sprintf("https://example/c/%d", f.next)}
		f.comments = append(f.comments, c)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(c)
	case r.Method == "PATCH":
		if f.patch403 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			return
		}
		var id int64
		_, _ = fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &id)
		for i := range f.comments {
			if f.comments[i].ID == id {
				f.comments[i].Body = in.Body
				_ = json.NewEncoder(w).Encode(f.comments[i])
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func newClient(t *testing.T, f *fakeGitHub) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{Token: "ghs_secret", APIURL: srv.URL}
}

func TestUpsertCreatesThenEdits(t *testing.T) {
	f := &fakeGitHub{comments: []comment{{ID: 900, Body: "LGTM"}}, next: 900}
	c := newClient(t, f)
	ctx := context.Background()

	url1, err := c.UpsertComment(ctx, "o/r", 7, marker, marker+"\nfirst")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 2 || url1 == "" {
		t.Fatalf("first run should add one comment, got %d (%q)", len(f.comments), url1)
	}
	if _, err := c.UpsertComment(ctx, "o/r", 7, marker, marker+"\nsecond"); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 2 {
		t.Fatalf("second run must edit, not add: %d comments", len(f.comments))
	}
	if !strings.HasSuffix(f.comments[1].Body, "second") || f.comments[0].Body != "LGTM" {
		t.Errorf("wrong comment edited: %+v", f.comments)
	}
	if f.authz != "Bearer ghs_secret" {
		t.Errorf("authorization = %q", f.authz)
	}
}

func TestUpsertFindsMarkerOnLaterPage(t *testing.T) {
	f := &fakeGitHub{next: 1000}
	for i := 0; i < perPage; i++ {
		f.comments = append(f.comments, comment{ID: int64(i + 1), Body: "noise"})
	}
	f.comments = append(f.comments, comment{ID: 500, Body: marker + "\nold"})
	c := newClient(t, f)
	if _, err := c.UpsertComment(context.Background(), "o/r", 7, marker, marker+"\nnew"); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != perPage+1 || !strings.HasSuffix(f.comments[perPage].Body, "new") {
		t.Errorf("did not edit the comment on page 2")
	}
}

func TestUpsertFallsBackToNewCommentWhenEditIsForbidden(t *testing.T) {
	f := &fakeGitHub{comments: []comment{{ID: 5, Body: marker + "\nsomeone else's"}}, patch403: true, next: 5}
	c := newClient(t, f)
	if _, err := c.UpsertComment(context.Background(), "o/r", 7, marker, marker+"\nmine"); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 2 {
		t.Errorf("expected a new comment after the 403, got %d", len(f.comments))
	}
}

func TestUpsertValidation(t *testing.T) {
	c := &Client{Token: "t"}
	ctx := context.Background()
	for name, fn := range map[string]func() error{
		"bad repo":   func() error { _, e := c.UpsertComment(ctx, "no-slash", 1, marker, marker); return e },
		"path trick": func() error { _, e := c.UpsertComment(ctx, "o/r/../../x", 1, marker, marker); return e },
		"bad pr":     func() error { _, e := c.UpsertComment(ctx, "o/r", 0, marker, marker); return e },
		"no marker":  func() error { _, e := c.UpsertComment(ctx, "o/r", 1, marker, "body"); return e },
		"no token":   func() error { _, e := (&Client{}).UpsertComment(ctx, "o/r", 1, marker, marker); return e },
	} {
		if fn() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestUpsertSurfacesGitHubErrorWithoutLeakingToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()
	_, err := (&Client{Token: "ghs_secret", APIURL: srv.URL}).UpsertComment(context.Background(), "o/r", 1, marker, marker)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "ghs_secret") {
		t.Error("error leaks the token")
	}
}

func fakeOAuthServer(t *testing.T, tokenBody string, userStatus int) (*OAuth, *url.Values) {
	t.Helper()
	got := &url.Values{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenBody))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(userStatus)
		_, _ = w.Write([]byte(`{"id":4242,"login":"Octo-Cat","avatar_url":"https://a/x.png"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &OAuth{ClientID: "cid", ClientSecret: "csecret", RedirectURL: "https://rg.example/auth/github/callback",
		AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", APIURL: srv.URL}, got
}

func TestOAuthLoginURL(t *testing.T) {
	o := &OAuth{ClientID: "cid", RedirectURL: "https://rg.example/cb"}
	u, err := url.Parse(o.LoginURL("st&ate"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "github.com" || u.Path != "/login/oauth/authorize" || q.Get("client_id") != "cid" ||
		q.Get("redirect_uri") != "https://rg.example/cb" || q.Get("state") != "st&ate" {
		t.Errorf("url = %s", u)
	}
	if q.Has("scope") {
		t.Error("no scope should be requested")
	}
}

func TestOAuthExchangeAndUser(t *testing.T) {
	o, form := fakeOAuthServer(t, `{"access_token":"tok123","token_type":"bearer"}`, 200)
	ctx := context.Background()
	tok, err := o.Exchange(ctx, "the-code")
	if err != nil || tok != "tok123" {
		t.Fatalf("exchange = %q, %v", tok, err)
	}
	if form.Get("code") != "the-code" || form.Get("client_secret") != "csecret" || form.Get("redirect_uri") == "" {
		t.Errorf("token request form = %v", *form)
	}
	u, err := o.User(ctx, tok)
	if err != nil || u.ID != 4242 || u.Login != "Octo-Cat" {
		t.Fatalf("user = %+v, %v", u, err)
	}
}

func TestOAuthFailures(t *testing.T) {
	ctx := context.Background()
	o, _ := fakeOAuthServer(t, `{"error":"bad_verification_code"}`, 200)
	if _, err := o.Exchange(ctx, "x"); err == nil || !strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("a 200 with an error field must fail: %v", err)
	}
	if _, err := o.Exchange(ctx, ""); err == nil {
		t.Error("empty code must fail")
	}
	o, _ = fakeOAuthServer(t, `{}`, 200)
	if _, err := o.Exchange(ctx, "x"); err == nil {
		t.Error("missing access_token must fail")
	}
	o, _ = fakeOAuthServer(t, `{"access_token":"tok123"}`, 500)
	if _, err := o.User(ctx, "tok123"); err == nil {
		t.Error("user endpoint failure must fail")
	}
	if _, err := o.User(ctx, "wrong"); err == nil {
		t.Error("rejected token must fail")
	}
}
