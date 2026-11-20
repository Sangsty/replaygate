package config

import (
	"strings"
	"testing"
)

func TestEnv(t *testing.T) {
	t.Setenv("RG_TEST_SET", "  value  ")
	t.Setenv("RG_TEST_EMPTY", "   ")

	tests := []struct {
		name    string
		run     func(e *Env) string
		want    string
		wantErr []string // substrings expected in Err(); nil means no error
	}{
		{"require trims", func(e *Env) string { return e.Require("RG_TEST_SET") }, "value", nil},
		{"require missing", func(e *Env) string { return e.Require("RG_TEST_MISSING") }, "", []string{"RG_TEST_MISSING"}},
		{"require blank counts as missing", func(e *Env) string { return e.Require("RG_TEST_EMPTY") }, "", []string{"RG_TEST_EMPTY"}},
		{"default used when unset", func(e *Env) string { return e.Default("RG_TEST_MISSING", ":8080") }, ":8080", nil},
		{"default used when blank", func(e *Env) string { return e.Default("RG_TEST_EMPTY", "x") }, "x", nil},
		{"default ignored when set", func(e *Env) string { return e.Default("RG_TEST_SET", "x") }, "value", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e Env
			if got := tt.run(&e); got != tt.want {
				t.Errorf("value = %q, want %q", got, tt.want)
			}
			err := e.Err()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			for _, sub := range tt.wantErr {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not mention %q", err, sub)
				}
			}
		})
	}
}

func TestEnvReportsAllMissing(t *testing.T) {
	var e Env
	e.Require("RG_TEST_A")
	e.Require("RG_TEST_B")
	err := e.Err()
	if err == nil || !strings.Contains(err.Error(), "RG_TEST_A, RG_TEST_B") {
		t.Fatalf("error = %v, want both variables listed", err)
	}
}
