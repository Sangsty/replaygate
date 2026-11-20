package main

import (
	"strings"
	"testing"
)

const goodSecret = "0123456789abcdef0123456789abcdef-test-secret"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func full() map[string]string {
	return map[string]string{
		"RG_GITHUB_CLIENT_ID": "cid", "RG_GITHUB_CLIENT_SECRET": "csec",
		"RG_JWT_SECRET": goodSecret, "RG_PUBLIC_URL": "https://rg.example.com/",
	}
}

func TestAuthFromEnv(t *testing.T) {
	if cfg, err := authFromEnv(env(nil)); cfg != nil || err != nil {
		t.Errorf("nothing set: cfg=%v err=%v, want sign-in off", cfg, err)
	}

	// The server's own address is set in every deployment, including the ones
	// that have no OAuth app yet; on its own it must not switch sign-in on.
	for _, m := range []map[string]string{
		{"RG_PUBLIC_URL": "https://rg.example.com"},
		{"RG_PUBLIC_URL": "https://rg.example.com", "RG_GITHUB_CLIENT_ID": "", "RG_GITHUB_CLIENT_SECRET": " ", "RG_JWT_SECRET": ""},
	} {
		if cfg, err := authFromEnv(env(m)); cfg != nil || err != nil {
			t.Errorf("only RG_PUBLIC_URL set: cfg=%v err=%v, want sign-in off and no error", cfg, err)
		}
	}

	cfg, err := authFromEnv(env(full()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OAuth.RedirectURL != "https://rg.example.com/auth/github/callback" || cfg.DashboardURL != "https://rg.example.com/" ||
		!cfg.SecureCookies || cfg.Sessions.TTL().Hours() != 24 || cfg.OAuth.ClientSecret != "csec" {
		t.Errorf("cfg = %+v oauth = %+v", cfg, cfg.OAuth)
	}

	local := full()
	local["RG_PUBLIC_URL"] = "http://localhost:5173"
	local["RG_DASHBOARD_URL"] = "http://localhost:5173/app"
	local["RG_SESSION_TTL"] = "2h"
	cfg, err = authFromEnv(env(local))
	if err != nil || cfg.SecureCookies || cfg.DashboardURL != "http://localhost:5173/app" || cfg.Sessions.TTL().Hours() != 2 {
		t.Errorf("local cfg = %+v err = %v (http must not set Secure cookies)", cfg, err)
	}
}

func TestAuthFromEnvGitHubEnterprise(t *testing.T) {
	m := full()
	m["RG_GITHUB_BASE_URL"] = "https://ghe.corp.example/"
	m["RG_GITHUB_API_URL"] = "https://ghe.corp.example/api/v3"
	cfg, err := authFromEnv(env(m))
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.OAuth
	if o.AuthorizeURL != "https://ghe.corp.example/login/oauth/authorize" || o.TokenURL != "https://ghe.corp.example/login/oauth/access_token" ||
		o.APIURL != "https://ghe.corp.example/api/v3" {
		t.Errorf("oauth = %+v", o)
	}
	cfg, _ = authFromEnv(env(full()))
	if cfg.OAuth.AuthorizeURL != "" || cfg.OAuth.APIURL != "" { // empty means the github.com defaults
		t.Errorf("defaults must stay empty: %+v", cfg.OAuth)
	}
	m["RG_GITHUB_API_URL"] = "ghe.corp.example"
	if _, err := authFromEnv(env(m)); err == nil || !strings.Contains(err.Error(), "RG_GITHUB_API_URL") {
		t.Errorf("err = %v", err)
	}
}

func TestAuthFromEnvRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"partly configured", func(m map[string]string) { delete(m, "RG_PUBLIC_URL"); delete(m, "RG_JWT_SECRET") }, "RG_JWT_SECRET, RG_PUBLIC_URL"},
		{"only the client id", func(m map[string]string) {
			delete(m, "RG_GITHUB_CLIENT_SECRET")
			delete(m, "RG_JWT_SECRET")
		}, "RG_GITHUB_CLIENT_SECRET, RG_JWT_SECRET"},
		{"only the jwt secret", func(m map[string]string) {
			delete(m, "RG_GITHUB_CLIENT_ID")
			delete(m, "RG_GITHUB_CLIENT_SECRET")
		}, "RG_GITHUB_CLIENT_ID, RG_GITHUB_CLIENT_SECRET"},
		{"secrets without a public url", func(m map[string]string) { delete(m, "RG_PUBLIC_URL") }, "RG_PUBLIC_URL"},
		{"short secret", func(m map[string]string) { m["RG_JWT_SECRET"] = "short" }, "RG_JWT_SECRET"},
		{"relative public url", func(m map[string]string) { m["RG_PUBLIC_URL"] = "rg.example.com" }, "RG_PUBLIC_URL"},
		{"bad dashboard url", func(m map[string]string) { m["RG_DASHBOARD_URL"] = "/app" }, "RG_DASHBOARD_URL"},
		{"bad ttl", func(m map[string]string) { m["RG_SESSION_TTL"] = "forever" }, "RG_SESSION_TTL"},
		{"ttl too short", func(m map[string]string) { m["RG_SESSION_TTL"] = "5s" }, "RG_SESSION_TTL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := full()
			tt.mutate(m)
			cfg, err := authFromEnv(env(m))
			if err == nil || cfg != nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("cfg=%v err=%v, want an error mentioning %q", cfg, err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), goodSecret) {
				t.Error("error leaks the secret")
			}
		})
	}
}
