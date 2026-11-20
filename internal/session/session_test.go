package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

const secret = "0123456789abcdef0123456789abcdef-test-secret"

func newMgr(t *testing.T, now *time.Time) *Manager {
	t.Helper()
	m, err := NewManager(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if now != nil {
		m.now = func() time.Time { return *now }
	}
	return m
}

func enc(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func forge(secret, hdr, payload string) string {
	in := enc(hdr) + "." + enc(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(in))
	return in + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestRoundTrip(t *testing.T) {
	m := newMgr(t, nil)
	tok, err := m.Issue("user-1", 4242, "octocat")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(tok, ".") != 2 {
		t.Fatalf("not a JWT: %s", tok)
	}
	c, err := m.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserID != "user-1" || c.GitHubID != 4242 || c.Login != "octocat" || c.Issuer != "replaygate" {
		t.Errorf("claims = %+v", c)
	}
	if c.Expires-c.IssuedAt != 3600 {
		t.Errorf("lifetime = %ds", c.Expires-c.IssuedAt)
	}
}

func TestExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newMgr(t, &now)
	tok, _ := m.Issue("u", 1, "a")
	now = now.Add(time.Hour - time.Second)
	if _, err := m.Verify(tok); err != nil {
		t.Errorf("token should still be valid: %v", err)
	}
	now = now.Add(time.Second)
	if _, err := m.Verify(tok); err == nil {
		t.Error("token must expire exactly at its exp")
	}
}

func TestRejects(t *testing.T) {
	m := newMgr(t, nil)
	good, _ := m.Issue("u", 1, "a")
	future := time.Now().Add(time.Hour).Unix()
	payload := func(extra string) string {
		return `{"sub":"u","gid":1,"login":"a","iss":"replaygate","exp":` + itoa(future) + extra + `}`
	}
	hs := `{"alg":"HS256","typ":"JWT"}`

	tests := []struct{ name, tok string }{
		{"empty", ""},
		{"garbage", "not a token"},
		{"two parts", "a.b"},
		{"four parts", good + ".x"},
		{"truncated signature", good[:len(good)-4]},
		{"tampered payload", strings.Split(good, ".")[0] + "." + enc(payload(`,"gid":2`)) + "." + strings.Split(good, ".")[2]},
		{"wrong secret", forge("another-secret-another-secret-another!", hs, payload(""))},
		{"alg none", enc(`{"alg":"none","typ":"JWT"}`) + "." + enc(payload("")) + "."},
		{"alg none with signature", forge(secret, `{"alg":"none","typ":"JWT"}`, payload(""))},
		{"alg HS512", forge(secret, `{"alg":"HS512","typ":"JWT"}`, payload(""))},
		{"header with extra field", forge(secret, `{"alg":"HS256","typ":"JWT","kid":"x"}`, payload(""))},
		{"wrong issuer", forge(secret, hs, `{"sub":"u","gid":1,"login":"a","iss":"evil","exp":`+itoa(future)+`}`)},
		{"missing exp", forge(secret, hs, `{"sub":"u","gid":1,"login":"a","iss":"replaygate"}`)},
		{"expired", forge(secret, hs, `{"sub":"u","gid":1,"login":"a","iss":"replaygate","exp":1}`)},
		{"missing subject", forge(secret, hs, `{"gid":1,"login":"a","iss":"replaygate","exp":`+itoa(future)+`}`)},
		{"zero github id", forge(secret, hs, `{"sub":"u","gid":0,"login":"a","iss":"replaygate","exp":`+itoa(future)+`}`)},
		{"payload not json", forge(secret, hs, `nope`)},
		{"oversized", strings.Repeat("a", maxTokenLen+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := m.Verify(tt.tok); err == nil {
				t.Errorf("token was accepted: %s", tt.tok)
			}
		})
	}
	// The forging helper itself is sound: the same shape with the right key and
	// good claims is accepted.
	if _, err := m.Verify(forge(secret, hs, payload(""))); err != nil {
		t.Errorf("control token rejected: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestConstructorAndIssueValidation(t *testing.T) {
	if _, err := NewManager("short", time.Hour); err == nil {
		t.Error("short secret accepted")
	}
	if _, err := NewManager(secret, 0); err == nil {
		t.Error("zero ttl accepted")
	}
	m := newMgr(t, nil)
	for _, c := range []struct {
		id    string
		gid   int64
		login string
	}{{"", 1, "a"}, {"u", 0, "a"}, {"u", 1, ""}} {
		if _, err := m.Issue(c.id, c.gid, c.login); err == nil {
			t.Errorf("Issue(%+v) should fail", c)
		}
	}
	if m.TTL() != time.Hour {
		t.Errorf("TTL = %v", m.TTL())
	}
}
