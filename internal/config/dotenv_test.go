package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDotEnv(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		preset  map[string]string
		want    map[string]string
		wantErr string
	}{
		{
			name: "basic pairs, comments and blank lines",
			file: "# comment\n\nRG_T_A=one\nRG_T_B=two words\n",
			want: map[string]string{"RG_T_A": "one", "RG_T_B": "two words"},
		},
		{
			name: "quotes and export prefix",
			file: "export RG_T_A=\"quoted value\"\nRG_T_B='single'\n",
			want: map[string]string{"RG_T_A": "quoted value", "RG_T_B": "single"},
		},
		{
			name: "value may contain equals and hash",
			file: "RG_T_A=postgres://u:p#1@h/db?sslmode=disable\n",
			want: map[string]string{"RG_T_A": "postgres://u:p#1@h/db?sslmode=disable"},
		},
		{
			name: "windows line endings and BOM",
			file: "\uFEFFRG_T_A=one\r\nRG_T_B=two\r\n",
			want: map[string]string{"RG_T_A": "one", "RG_T_B": "two"},
		},
		{
			name:   "existing environment wins",
			file:   "RG_T_A=from-file\nRG_T_B=from-file\n",
			preset: map[string]string{"RG_T_A": "from-env"},
			want:   map[string]string{"RG_T_A": "from-env", "RG_T_B": "from-file"},
		},
		{
			name: "empty value is allowed",
			file: "RG_T_A=\n",
			want: map[string]string{"RG_T_A": ""},
		},
		{
			name:    "line without equals",
			file:    "RG_T_A=one\nnot a pair\n",
			wantErr: ".env:2",
		},
		{
			name:    "empty key",
			file:    "=value\n",
			wantErr: ".env:1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Register cleanup first so variables set by LoadDotEnv are restored.
			for _, k := range []string{"RG_T_A", "RG_T_B"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			for k, v := range tt.preset {
				t.Setenv(k, v)
			}
			err := LoadDotEnv(writeEnvFile(t, tt.file))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for k, want := range tt.want {
				if got, ok := os.LookupEnv(k); !ok || got != want {
					t.Errorf("%s = %q (set=%v), want %q", k, got, ok, want)
				}
			}
		})
	}
}

func TestLoadDotEnvMissingFileIsNotAnError(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatalf("missing file returned error: %v", err)
	}
}
