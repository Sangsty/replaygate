// Package config reads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Env reads variables and remembers which required ones were missing, so a
// service can report every configuration problem at once instead of one per run.
type Env struct {
	missing []string
}

// Require returns the trimmed value of key and records it as missing if empty.
func (e *Env) Require(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		e.missing = append(e.missing, key)
	}
	return v
}

// Default returns the trimmed value of key, or def if the variable is unset or empty.
func (e *Env) Default(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Err reports all required variables that were missing, or nil.
func (e *Env) Err() error {
	if len(e.missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing required environment variables: %s", strings.Join(e.missing, ", "))
}
