package normalize

import (
	"bytes"
	"testing"
)

func TestEndpoint(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"empty", "", "/"},
		{"root", "/", "/"},
		{"plain", "/users", "/users"},
		{"numeric id", "/users/123", "/users/{id}"},
		{"trailing slash", "/users/123/", "/users/{id}"},
		{"nested ids", "/users/123/orders/456", "/users/{id}/orders/{id}"},
		{"uuid lower", "/items/550e8400-e29b-41d4-a716-446655440000", "/items/{id}"},
		{"uuid upper", "/items/550E8400-E29B-41D4-A716-446655440000", "/items/{id}"},
		{"objectid", "/docs/507f1f77bcf86cd799439011", "/docs/{id}"},
		{"version segment kept", "/v2/users", "/v2/users"},
		{"word kept", "/users/me", "/users/me"},
		{"mixed alnum kept", "/users/12a", "/users/12a"},
		{"short hex kept", "/colors/ff00ff", "/colors/ff00ff"},
		{"almost uuid kept", "/items/550e8400-e29b-41d4-a716-44665544000", "/items/550e8400-e29b-41d4-a716-44665544000"},
		{"uuid wrong dash kept", "/items/550e8400xe29b-41d4-a716-446655440000", "/items/550e8400xe29b-41d4-a716-446655440000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Endpoint(tt.path); got != tt.want {
				t.Errorf("Endpoint(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestCanonicalQuery(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"sorted by key", "b=2&a=1", "a=1&b=2"},
		{"repeated key keeps value order", "tag=b&tag=a", "tag=b&tag=a"},
		{"key without value", "flag&a=1", "a=1&flag="},
		{"invalid escape returned unchanged", "a=%zz", "a=%zz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanonicalQuery(tt.raw); got != tt.want {
				t.Errorf("CanonicalQuery(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	base := Fingerprint("GET", "/users/1", "a=1&b=2", nil)

	if len(base) != 32 {
		t.Fatalf("fingerprint length = %d, want 32", len(base))
	}

	tests := []struct {
		name      string
		method    string
		path      string
		query     string
		body      []byte
		wantEqual bool
	}{
		{"identical", "GET", "/users/1", "a=1&b=2", nil, true},
		{"query reordered", "GET", "/users/1", "b=2&a=1", nil, true},
		{"empty body equals nil body", "GET", "/users/1", "a=1&b=2", []byte{}, true},
		{"different path", "GET", "/users/2", "a=1&b=2", nil, false},
		{"different query value", "GET", "/users/1", "a=1&b=3", nil, false},
		{"different method", "HEAD", "/users/1", "a=1&b=2", nil, false},
		{"different body", "GET", "/users/1", "a=1&b=2", []byte("x"), false},
		{"field boundary shifted", "GE", "T/users/1", "a=1&b=2", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Fingerprint(tt.method, tt.path, tt.query, tt.body)
			if bytes.Equal(got, base) != tt.wantEqual {
				t.Errorf("equal to base = %v, want %v", bytes.Equal(got, base), tt.wantEqual)
			}
		})
	}
}

func TestFingerprintRepeatedValueOrderMatters(t *testing.T) {
	a := Fingerprint("GET", "/x", "tag=a&tag=b", nil)
	b := Fingerprint("GET", "/x", "tag=b&tag=a", nil)
	if bytes.Equal(a, b) {
		t.Error("repeated-key value order should change the fingerprint")
	}
}
