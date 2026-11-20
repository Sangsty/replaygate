package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewKey(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Plaintext, "rg_") {
		t.Errorf("key %q lacks rg_ prefix", k.Plaintext)
	}
	if want := len("rg_") + 43; len(k.Plaintext) != want {
		t.Errorf("key length = %d, want %d", len(k.Plaintext), want)
	}
	if k.Prefix != k.Plaintext[:displayLen] {
		t.Errorf("prefix = %q, want first %d chars of key", k.Prefix, displayLen)
	}
	if !bytes.Equal(k.Hash, Hash(k.Plaintext)) {
		t.Error("stored hash does not match Hash(plaintext)")
	}
	if len(k.Hash) != 32 {
		t.Errorf("hash length = %d, want 32", len(k.Hash))
	}
}

func TestNewKeyUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		k, err := NewKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[k.Plaintext] {
			t.Fatalf("duplicate key generated: %s", k.Plaintext)
		}
		seen[k.Plaintext] = true
	}
}

func TestHash(t *testing.T) {
	if !bytes.Equal(Hash("rg_a"), Hash("rg_a")) {
		t.Error("Hash is not deterministic")
	}
	if bytes.Equal(Hash("rg_a"), Hash("rg_b")) {
		t.Error("different keys produced the same hash")
	}
}

func TestParseBearer(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
		wantOK bool
	}{
		{"valid", "Bearer rg_abc123", "rg_abc123", true},
		{"scheme case-insensitive", "bearer rg_abc123", "rg_abc123", true},
		{"surrounding spaces", "  Bearer   rg_abc123  ", "rg_abc123", true},
		{"empty header", "", "", false},
		{"no token", "Bearer", "", false},
		{"wrong scheme", "Basic rg_abc123", "", false},
		{"missing prefix", "Bearer abc123", "", false},
		{"prefix only", "Bearer rg_", "", false},
		{"token without scheme", "rg_abc123", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseBearer(tt.header)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("ParseBearer(%q) = (%q, %v), want (%q, %v)", tt.header, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
