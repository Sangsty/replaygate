// Package sanitize removes secrets and PII from recorded traffic before it is stored.
package sanitize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Rules is the per-project redaction configuration, stored as JSON on the project.
type Rules struct {
	// Headers are header names whose values are replaced. Matching is case-insensitive.
	Headers []string `json:"headers"`
	// QueryParams are query parameter names whose values are replaced.
	QueryParams []string `json:"query_params"`
	// JSONPaths select values to replace inside JSON bodies (see parsePath for the syntax).
	JSONPaths []string `json:"json_paths"`
}

// hardFloor lists headers that are always redacted, even if a project's rules
// forget to list them.
var hardFloor = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
}

// Sanitizer applies compiled Rules. It is immutable after New and safe for
// concurrent use.
type Sanitizer struct {
	headers map[string]struct{}
	query   map[string]struct{}
	paths   [][]step
}

// New compiles rules. It fails on an invalid JSONPath so a typo in a rule is
// caught when rules are loaded, not silently ignored while PII is stored.
func New(r Rules) (*Sanitizer, error) {
	s := &Sanitizer{
		headers: make(map[string]struct{}, len(hardFloor)+len(r.Headers)),
		query:   make(map[string]struct{}, len(r.QueryParams)),
	}
	for h := range hardFloor {
		s.headers[h] = struct{}{}
	}
	for _, h := range r.Headers {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			s.headers[h] = struct{}{}
		}
	}
	for _, q := range r.QueryParams {
		if q = strings.ToLower(strings.TrimSpace(q)); q != "" {
			s.query[q] = struct{}{}
		}
	}
	for _, raw := range r.JSONPaths {
		steps, err := parsePath(strings.TrimSpace(raw))
		if err != nil {
			return nil, err
		}
		s.paths = append(s.paths, steps)
	}
	return s, nil
}

// Headers returns a copy of in with lowercase names and redacted values for
// every sensitive header. The input is not modified.
func (s *Sanitizer) Headers(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for name, vals := range in {
		lower := strings.ToLower(name)
		if _, secret := s.headers[lower]; secret {
			out[lower] = []string{Redacted}
			continue
		}
		out[lower] = append(out[lower], vals...)
	}
	return out
}

// Query redacts the values of sensitive query parameters, keeping parameter
// order and every other byte of the query string as it was.
func (s *Sanitizer) Query(raw string) string {
	if raw == "" || len(s.query) == 0 {
		return raw
	}
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		name, err := url.QueryUnescape(key)
		if err != nil {
			name = key
		}
		if _, secret := s.query[strings.ToLower(name)]; secret {
			parts[i] = key + "=" + url.QueryEscape(Redacted)
		}
	}
	return strings.Join(parts, "&")
}

// Body redacts the configured JSONPaths in a JSON body and returns compact JSON
// with sorted object keys. Numbers keep their original text (1.0 stays 1.0).
//
// With no JSONPath rules the body is returned unchanged. With rules, a body
// that is not valid JSON cannot be checked for PII, so it is dropped (nil)
// rather than stored unsanitized.
func (s *Sanitizer) Body(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	if len(s.paths) == 0 {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil
	}
	if _, err := dec.Token(); err != io.EOF { // trailing data after the document
		return nil
	}
	for _, steps := range s.paths {
		redact(doc, steps)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Backstop deletes the credential headers every project must never store. The
// API applies it on ingest as defense in depth, in case a recorder is outdated
// or misconfigured. It modifies h in place.
func Backstop(h map[string][]string) {
	for name := range h {
		if _, secret := hardFloor[strings.ToLower(name)]; secret {
			delete(h, name)
		}
	}
}

// DefaultRules is what a new project starts with. It matches the column default
// in the schema.
func DefaultRules() Rules {
	return Rules{
		Headers:     []string{"authorization", "cookie", "set-cookie", "x-api-key"},
		QueryParams: []string{},
		JSONPaths:   []string{},
	}
}

// String summarises the rules without printing any values, for logs.
func (r Rules) String() string {
	return fmt.Sprintf("headers=%d query_params=%d json_paths=%d", len(r.Headers), len(r.QueryParams), len(r.JSONPaths))
}
