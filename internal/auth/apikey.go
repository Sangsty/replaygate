// Package auth creates and verifies per-project API keys.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	keyPrefix    = "rg_"
	keyRandBytes = 32
	// displayLen is how much of the key is stored in clear for the dashboard.
	displayLen = 8
)

// Key is a freshly generated API key. Plaintext is shown to the user once and
// never stored; only Hash and Prefix go into the database.
type Key struct {
	Plaintext string
	Prefix    string
	Hash      []byte
}

// NewKey generates a 256-bit random key, e.g. "rg_<43 base64url chars>".
func NewKey() (Key, error) {
	b := make([]byte, keyRandBytes)
	if _, err := rand.Read(b); err != nil {
		return Key{}, fmt.Errorf("generate api key: %w", err)
	}
	plain := keyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return Key{Plaintext: plain, Prefix: plain[:displayLen], Hash: Hash(plain)}, nil
}

// Hash returns the sha256 of a key. A fast hash is sufficient because keys are
// high-entropy random values, not human-chosen passwords.
func Hash(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// ParseBearer extracts the API key from an "Authorization: Bearer <key>" header.
func ParseBearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, keyPrefix) || len(token) <= len(keyPrefix) {
		return "", false
	}
	return token, true
}
