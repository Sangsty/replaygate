package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sangsty/replaygate/internal/github"
	"github.com/Sangsty/replaygate/internal/session"
)

const (
	sessionCookie = "rg_session"
	stateCookie   = "rg_oauth_state"
	stateTTL      = 10 * time.Minute
)

// AuthConfig turns on dashboard sign-in with GitHub.
type AuthConfig struct {
	Sessions *session.Manager
	OAuth    *github.OAuth
	// DashboardURL is where the browser lands after sign-in, success or not.
	DashboardURL string
	// SecureCookies marks cookies Secure. Turn it on whenever the site is
	// served over HTTPS, which is always except for local development.
	SecureCookies bool
}

// WithAuth enables GitHub sign-in and session-authenticated reads.
func WithAuth(c AuthConfig) Option { return func(a *api) { a.auth = &c } }

// githubLogin starts the OAuth flow. The random state, kept in a short-lived
// cookie and echoed back by GitHub, ties the callback to this browser; without
// it anyone could trick a user into signing in as the attacker.
func (a *api) githubLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken(24)
	if err != nil {
		a.log.Error("generate oauth state", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/auth", MaxAge: int(stateTTL.Seconds()),
		HttpOnly: true, Secure: a.auth.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.auth.OAuth.LoginURL(state), http.StatusFound)
}

// githubCallback finishes sign-in. Whatever happens, the browser is sent back
// to the dashboard (with ?login_error=... on failure) instead of being left on
// a bare error page.
func (a *api) githubCallback(w http.ResponseWriter, r *http.Request) {
	// The state cookie is single-use.
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/auth", MaxAge: -1,
		HttpOnly: true, Secure: a.auth.SecureCookies, SameSite: http.SameSiteLaxMode})

	q := r.URL.Query()
	cookie, err := r.Cookie(stateCookie)
	if err != nil || cookie.Value == "" || q.Get("state") == "" ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(q.Get("state"))) != 1 {
		a.loginFailed(w, r, "state", "oauth state mismatch")
		return
	}
	if q.Get("error") != "" { // the user pressed "cancel" on GitHub
		a.loginFailed(w, r, "denied", "user declined")
		return
	}

	token, err := a.auth.OAuth.Exchange(r.Context(), q.Get("code"))
	if err != nil {
		a.loginFailed(w, r, "failed", err.Error())
		return
	}
	u, err := a.auth.OAuth.User(r.Context(), token)
	if err != nil {
		a.loginFailed(w, r, "failed", err.Error())
		return
	}
	userID, err := a.store.UpsertUser(r.Context(), u.ID, u.Login)
	if err != nil {
		a.loginFailed(w, r, "failed", err.Error())
		return
	}
	jwt, err := a.auth.Sessions.Issue(userID, u.ID, u.Login)
	if err != nil {
		a.loginFailed(w, r, "failed", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: jwt, Path: "/", MaxAge: int(a.auth.Sessions.TTL().Seconds()),
		HttpOnly: true, Secure: a.auth.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	a.log.Info("user signed in", "login", u.Login)
	http.Redirect(w, r, a.auth.DashboardURL, http.StatusFound)
}

func (a *api) loginFailed(w http.ResponseWriter, r *http.Request, code, detail string) {
	a.log.Warn("sign-in failed", "code", code, "detail", detail)
	dest, err := url.Parse(a.auth.DashboardURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "sign-in failed")
		return
	}
	q := dest.Query()
	q.Set("login_error", code)
	dest.RawQuery = q.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

func (a *api) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.auth.SecureCookies, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

// claims returns the signed-in user, if the request carries a valid session.
func (a *api) claims(r *http.Request) (session.Claims, bool) {
	if a.auth == nil {
		return session.Claims{}, false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session.Claims{}, false
	}
	cl, err := a.auth.Sessions.Verify(c.Value)
	return cl, err == nil
}

// requireKeyOrSession lets a request through if it carries a valid project API
// key, or a valid session of someone who may open the project.
//
// Cookies are sent by the browser automatically, which is what makes
// cookie-authenticated writes vulnerable to cross-site request forgery. This
// middleware therefore only accepts a session for GET and HEAD; every route
// behind it is read-only, and the check keeps it that way if one is added.
func (a *api) requireKeyOrSession(next http.Handler) http.Handler {
	byKey := a.requireProjectKey(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Authorization") != "" {
			byKey.ServeHTTP(w, r)
			return
		}
		cl, ok := a.claims(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "sign in, or provide an API key")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "sessions are read-only")
			return
		}
		pid := chi.URLParam(r, "pid")
		if !uuidRe.MatchString(pid) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		allowed, err := a.store.CanAccessProject(r.Context(), pid, cl.UserID, cl.Login)
		if err != nil {
			a.log.Error("check project access", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !allowed {
			// 404, not 403: do not confirm that someone else's project exists.
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *api) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	cl, ok := a.claims(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user_id": cl.UserID, "github_id": cl.GitHubID, "login": cl.Login})
}

func (a *api) listProjects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	cl, ok := a.claims(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	projects, err := a.store.ProjectsForUser(r.Context(), cl.UserID, cl.Login)
	if err != nil {
		a.storeError(w, "list projects", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
