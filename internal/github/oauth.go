package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth is the "Sign in with GitHub" web flow. No scopes are requested: the
// dashboard only needs to know who the user is, which the public profile says.
type OAuth struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string // must match the callback URL registered with the OAuth app

	// Overridable for tests and GitHub Enterprise. Defaults are github.com.
	AuthorizeURL string
	TokenURL     string
	APIURL       string
	HTTP         *http.Client
}

// User is the part of the GitHub profile replaygate keeps.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

func (o *OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// LoginURL is where to send the browser to start signing in. state must be a
// fresh random value that the callback checks against the user's cookie.
func (o *OAuth) LoginURL(state string) string {
	q := url.Values{}
	q.Set("client_id", o.ClientID)
	q.Set("redirect_uri", o.RedirectURL)
	q.Set("state", state)
	q.Set("allow_signup", "true")
	return orDefault(o.AuthorizeURL, "https://github.com/login/oauth/authorize") + "?" + q.Encode()
}

// Exchange trades the one-time code from the callback for an access token.
func (o *OAuth) Exchange(ctx context.Context, code string) (string, error) {
	if code == "" {
		return "", errors.New("empty code")
	}
	form := url.Values{}
	form.Set("client_id", o.ClientID)
	form.Set("client_secret", o.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", o.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		orDefault(o.TokenURL, "https://github.com/login/oauth/access_token"), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "replaygate")

	resp, err := o.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exchange code: GitHub answered %d", resp.StatusCode)
	}
	// GitHub reports a bad or reused code with HTTP 200 and an "error" field.
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("exchange code: decode response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("exchange code: %s", out.Error)
	}
	if out.AccessToken == "" {
		return "", errors.New("exchange code: no access token in response")
	}
	return out.AccessToken, nil
}

// User fetches the signed-in user's profile.
func (o *OAuth) User(ctx context.Context, token string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orDefault(o.APIURL, defaultAPIURL)+"/user", nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "replaygate")

	resp, err := o.client().Do(req)
	if err != nil {
		return User{}, fmt.Errorf("fetch user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return User{}, fmt.Errorf("fetch user: GitHub answered %d", resp.StatusCode)
	}
	var u User
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return User{}, fmt.Errorf("fetch user: decode response: %w", err)
	}
	if u.ID <= 0 || u.Login == "" {
		return User{}, errors.New("fetch user: response has no id or login")
	}
	return u, nil
}
