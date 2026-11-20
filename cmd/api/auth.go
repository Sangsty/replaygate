package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Sangsty/replaygate/internal/github"
	"github.com/Sangsty/replaygate/internal/server"
	"github.com/Sangsty/replaygate/internal/session"
)

// authFromEnv builds the dashboard sign-in configuration. It returns nil when
// none of the three secrets (client id, client secret, JWT secret) is set, so
// sign-in is off, and an error when only some of the four settings are,
// because a half-configured login is a mistake that should stop the server,
// not silently disable the dashboard.
//
//	RG_GITHUB_CLIENT_ID, RG_GITHUB_CLIENT_SECRET  from the GitHub OAuth app
//	RG_JWT_SECRET    at least 32 bytes of random data (openssl rand -base64 48)
//	RG_PUBLIC_URL    the address users type, e.g. https://replaygate.example.com;
//	                 the OAuth callback is RG_PUBLIC_URL/auth/github/callback
//	RG_DASHBOARD_URL where to send users after sign-in (default RG_PUBLIC_URL/)
//	RG_SESSION_TTL   how long a sign-in lasts (default 24h)
//	RG_GITHUB_BASE_URL, RG_GITHUB_API_URL
//	                 for GitHub Enterprise Server (defaults: https://github.com
//	                 and https://api.github.com)
func authFromEnv(getenv func(string) string) (*server.AuthConfig, error) {
	names := []string{"RG_GITHUB_CLIENT_ID", "RG_GITHUB_CLIENT_SECRET", "RG_JWT_SECRET", "RG_PUBLIC_URL"}
	vals := map[string]string{}
	var missing []string
	for _, n := range names {
		if v := strings.TrimSpace(getenv(n)); v != "" {
			vals[n] = v
		} else {
			missing = append(missing, n)
		}
	}
	// RG_PUBLIC_URL alone does not turn sign-in on: deployments always set it
	// (it is the server's own address), long before there is an OAuth app. Only
	// the three secrets decide, and then everything must be present.
	if vals["RG_GITHUB_CLIENT_ID"] == "" && vals["RG_GITHUB_CLIENT_SECRET"] == "" && vals["RG_JWT_SECRET"] == "" {
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("dashboard sign-in is partly configured; also set: %s", strings.Join(missing, ", "))
	}

	pub, err := url.Parse(vals["RG_PUBLIC_URL"])
	if err != nil || (pub.Scheme != "http" && pub.Scheme != "https") || pub.Host == "" {
		return nil, fmt.Errorf("RG_PUBLIC_URL must be an absolute http(s) URL, got %q", vals["RG_PUBLIC_URL"])
	}
	base := strings.TrimRight(pub.String(), "/")

	dashboard := base + "/"
	if v := strings.TrimSpace(getenv("RG_DASHBOARD_URL")); v != "" {
		d, err := url.Parse(v)
		if err != nil || (d.Scheme != "http" && d.Scheme != "https") || d.Host == "" {
			return nil, fmt.Errorf("RG_DASHBOARD_URL must be an absolute http(s) URL, got %q", v)
		}
		dashboard = d.String()
	}

	ttl := 24 * time.Hour
	if v := strings.TrimSpace(getenv("RG_SESSION_TTL")); v != "" {
		if ttl, err = time.ParseDuration(v); err != nil || ttl < time.Minute {
			return nil, fmt.Errorf("RG_SESSION_TTL must be a duration of at least 1m, such as 24h, got %q", v)
		}
	}
	mgr, err := session.NewManager(vals["RG_JWT_SECRET"], ttl)
	if err != nil {
		return nil, fmt.Errorf("RG_JWT_SECRET: %w", err)
	}
	oauth := &github.OAuth{
		ClientID:     vals["RG_GITHUB_CLIENT_ID"],
		ClientSecret: vals["RG_GITHUB_CLIENT_SECRET"],
		RedirectURL:  base + "/auth/github/callback",
	}
	if v, err := optionalURL(getenv, "RG_GITHUB_BASE_URL"); err != nil {
		return nil, err
	} else if v != "" {
		oauth.AuthorizeURL, oauth.TokenURL = v+"/login/oauth/authorize", v+"/login/oauth/access_token"
	}
	if v, err := optionalURL(getenv, "RG_GITHUB_API_URL"); err != nil {
		return nil, err
	} else if v != "" {
		oauth.APIURL = v
	}
	return &server.AuthConfig{
		Sessions:      mgr,
		DashboardURL:  dashboard,
		SecureCookies: pub.Scheme == "https",
		OAuth:         oauth,
	}, nil
}

// optionalURL returns the variable as an absolute http(s) URL without a
// trailing slash, or "" if it is unset.
func optionalURL(getenv func(string) string, name string) (string, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s must be an absolute http(s) URL, got %q", name, v)
	}
	return strings.TrimRight(u.String(), "/"), nil
}
