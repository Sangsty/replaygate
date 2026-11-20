// Package session issues and verifies the login token the dashboard keeps in
// a cookie: a standard HS256 JSON Web Token.
//
// It is written against the standard library on purpose. The surface is small
// (one algorithm, one key, fixed claims) and refusing everything else is the
// point: the verifier accepts exactly the tokens Issue produces. In particular
// it never reads the algorithm from the token, so "alg":"none" and
// algorithm-confusion tricks have nothing to work with.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	issuer = "replaygate"
	// MinSecretLen is the shortest signing secret accepted. HMAC-SHA256 gains
	// nothing from keys longer than 64 bytes, and short keys can be guessed.
	MinSecretLen = 32
	maxTokenLen  = 4096
)

// ErrInvalid is returned for any token that must not be trusted: malformed,
// wrongly signed, expired, or from another issuer. Callers need not tell the
// cases apart, and telling a client which one failed helps only an attacker.
var ErrInvalid = errors.New("invalid session")

// Claims is who the token vouches for.
type Claims struct {
	UserID   string `json:"sub"`   // replaygate users.id
	GitHubID int64  `json:"gid"`   // GitHub's numeric user id
	Login    string `json:"login"` // GitHub login at sign-in time
	Issuer   string `json:"iss"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// Manager signs and verifies tokens.
type Manager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewManager returns a Manager. The secret must be at least MinSecretLen bytes
// of random data (for example `openssl rand -base64 48`).
func NewManager(secret string, ttl time.Duration) (*Manager, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("session secret must be at least %d bytes; generate one with: openssl rand -base64 48", MinSecretLen)
	}
	if ttl <= 0 {
		return nil, errors.New("session lifetime must be positive")
	}
	return &Manager{secret: []byte(secret), ttl: ttl, now: time.Now}, nil
}

// TTL is how long a token stays valid.
func (m *Manager) TTL() time.Duration { return m.ttl }

// the header is constant, so it is encoded once.
var header = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// Issue returns a signed token for the user.
func (m *Manager) Issue(userID string, githubID int64, login string) (string, error) {
	if userID == "" || githubID <= 0 || login == "" {
		return "", errors.New("session: user id, github id and login are required")
	}
	now := m.now()
	payload, err := json.Marshal(Claims{
		UserID: userID, GitHubID: githubID, Login: login, Issuer: issuer,
		IssuedAt: now.Unix(), Expires: now.Add(m.ttl).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signingInput + "." + m.sign(signingInput), nil
}

// Verify checks the token and returns its claims.
func (m *Manager) Verify(token string) (Claims, error) {
	if len(token) == 0 || len(token) > maxTokenLen {
		return Claims{}, ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalid
	}
	// The header must be byte-for-byte the one we issue. Comparing the encoded
	// form is stricter than parsing it and cannot be fooled by alg tricks.
	if parts[0] != header {
		return Claims{}, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	want, _ := base64.RawURLEncoding.DecodeString(m.sign(parts[0] + "." + parts[1]))
	if !hmac.Equal(got, want) { // constant time
		return Claims{}, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, ErrInvalid
	}
	if c.Issuer != issuer || c.UserID == "" || c.GitHubID <= 0 || c.Login == "" || c.Expires <= m.now().Unix() {
		return Claims{}, ErrInvalid
	}
	return c, nil
}

func (m *Manager) sign(signingInput string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(signingInput))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
