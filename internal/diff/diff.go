// Package diff compares a baseline and a candidate JSON response and reports
// the differences as typed, severity-ranked findings.
//
// Severity model:
//
//	breaking  a consumer can break: status changed, field removed, type changed,
//	          value changed (configurable), request failed
//	additive  a new field appeared; consumers ignoring unknown fields are fine
//	latency   candidate is slower; reported but does not fail the gate by default
package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Severity ranks how serious a finding is.
type Severity string

const (
	Breaking Severity = "breaking"
	Additive Severity = "additive"
	Latency  Severity = "latency"
)

// Kind is the type of change. The values match the CHECK constraint on diffs.kind.
type Kind string

const (
	StatusChanged     Kind = "status_changed"
	FieldRemoved      Kind = "field_removed"
	TypeChanged       Kind = "type_changed"
	ValueChanged      Kind = "value_changed"
	FieldAdded        Kind = "field_added"
	LatencyRegression Kind = "latency_regression"
	RequestFailed     Kind = "request_failed"
)

// Finding is one difference between baseline and candidate.
type Finding struct {
	Severity  Severity        `json:"severity"`
	Kind      Kind            `json:"kind"`
	Path      string          `json:"json_path"`
	Baseline  json.RawMessage `json:"baseline_value,omitempty"`
	Candidate json.RawMessage `json:"candidate_value,omitempty"`
}

// Config controls the comparison. The zero value is usable: arrays compare in
// order, floats compare exactly, and a changed value is breaking.
type Config struct {
	// FloatTolerance treats two numbers as equal when their difference is at
	// most this absolute amount.
	FloatTolerance float64 `json:"float_tolerance"`
	// ArrayKeys maps an array's path (e.g. "$.items") to the object field that
	// identifies an element. Such arrays compare as sets keyed by that field.
	ArrayKeys map[string]string `json:"array_keys"`
	// UnorderedArrays lists array paths whose scalar elements compare as
	// multisets (order ignored).
	UnorderedArrays []string `json:"unordered_arrays"`
	// ValueChangeSeverity is the severity of value_changed. Default breaking.
	ValueChangeSeverity Severity `json:"value_change_severity"`
	// IgnorePaths are JSON paths (noise or user-declared) excluded from the diff.
	// "[*]" in a pattern matches any array index or element key.
	IgnorePaths []string `json:"ignore_paths"`
	// LatencyFactor flags latency_regression when candidate latency exceeds
	// baseline * LatencyFactor and the absolute increase exceeds LatencyMinMS.
	// Zero disables latency findings.
	LatencyFactor float64 `json:"latency_factor"`
	LatencyMinMS  int     `json:"latency_min_ms"`
}

// Response is what a replay pass observed for one request.
type Response struct {
	Status    int
	Body      []byte
	LatencyMS int
	Err       string // non-empty when the request itself failed (timeout, refused)
}

// maxDepth bounds recursion so a hostile or malformed body cannot blow the stack.
const maxDepth = 64

// Compare diffs baseline against candidate and returns findings sorted by
// severity, then path. A nil result means the responses are equivalent.
func Compare(base, cand Response, cfg Config) []Finding {
	var out []Finding

	switch {
	case base.Err != "" && cand.Err != "":
		return nil // both failed the same way: nothing the PR changed
	case cand.Err != "":
		return []Finding{{Severity: Breaking, Kind: RequestFailed, Candidate: quote(cand.Err)}}
	case base.Err != "":
		// Baseline failed but candidate answered. We cannot say it is broken.
		return nil
	}

	if base.Status != cand.Status {
		out = append(out, Finding{
			Severity: Breaking, Kind: StatusChanged,
			Baseline: intJSON(base.Status), Candidate: intJSON(cand.Status),
		})
	}

	if f := latencyFinding(base, cand, cfg); f != nil {
		out = append(out, *f)
	}

	// Bodies are only compared when the status codes agree: an error page
	// versus a payload is already one clear finding.
	if base.Status == cand.Status {
		out = append(out, compareBodies(base.Body, cand.Body, cfg)...)
	}

	sortFindings(out)
	return out
}

func compareBodies(a, b []byte, cfg Config) []Finding {
	if len(bytes.TrimSpace(a)) == 0 && len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	av, aerr := decode(a)
	bv, berr := decode(b)
	switch {
	case aerr != nil && berr != nil:
		// Neither side is JSON; compare raw bytes.
		if !bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b)) {
			return []Finding{{Severity: cfg.valueSeverity(), Kind: ValueChanged, Path: "$"}}
		}
		return nil
	case aerr != nil || berr != nil:
		return []Finding{{Severity: Breaking, Kind: TypeChanged, Path: "$"}}
	}
	d := &differ{cfg: cfg}
	d.walk("$", av, bv, 0)
	return d.out
}

func decode(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep big integers and decimals exact
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

type differ struct {
	cfg Config
	out []Finding
}

func (d *differ) add(sev Severity, kind Kind, path string, base, cand any) {
	if d.ignored(path) {
		return
	}
	f := Finding{Severity: sev, Kind: kind, Path: path}
	if base != nil {
		f.Baseline = marshalValue(base)
	}
	if cand != nil {
		f.Candidate = marshalValue(cand)
	}
	d.out = append(d.out, f)
}

func (d *differ) walk(path string, a, b any, depth int) {
	if d.ignored(path) {
		return
	}
	if depth > maxDepth {
		d.add(Breaking, ValueChanged, path, nil, nil)
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			d.add(Breaking, TypeChanged, path, a, b)
			return
		}
		d.walkObject(path, av, bv, depth)
	case []any:
		bv, ok := b.([]any)
		if !ok {
			d.add(Breaking, TypeChanged, path, a, b)
			return
		}
		d.walkArray(path, av, bv, depth)
	case json.Number:
		bv, ok := b.(json.Number)
		if !ok {
			d.add(Breaking, TypeChanged, path, a, b)
			return
		}
		if !numbersEqual(av, bv, d.cfg.FloatTolerance) {
			d.add(d.cfg.valueSeverity(), ValueChanged, path, a, b)
		}
	case string:
		bv, ok := b.(string)
		if !ok {
			d.add(Breaking, TypeChanged, path, a, b)
			return
		}
		if av != bv {
			d.add(d.cfg.valueSeverity(), ValueChanged, path, a, b)
		}
	case bool:
		bv, ok := b.(bool)
		if !ok {
			d.add(Breaking, TypeChanged, path, a, b)
			return
		}
		if av != bv {
			d.add(d.cfg.valueSeverity(), ValueChanged, path, a, b)
		}
	case nil:
		if b != nil {
			d.add(Breaking, TypeChanged, path, a, b)
		}
	}
}

func (d *differ) walkObject(path string, a, b map[string]any, depth int) {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := childPath(path, k)
		av, inA := a[k]
		bv, inB := b[k]
		switch {
		case inA && !inB:
			d.add(Breaking, FieldRemoved, p, av, nil)
		case !inA && inB:
			d.add(Additive, FieldAdded, p, nil, bv)
		default:
			d.walk(p, av, bv, depth+1)
		}
	}
}

func (d *differ) walkArray(path string, a, b []any, depth int) {
	if key, ok := d.cfg.ArrayKeys[path]; ok {
		d.walkKeyedArray(path, key, a, b, depth)
		return
	}
	if contains(d.cfg.UnorderedArrays, path) {
		d.walkMultiset(path, a, b)
		return
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		d.walk(fmt.Sprintf("%s[%d]", path, i), a[i], b[i], depth+1)
	}
	for i := n; i < len(a); i++ {
		d.add(Breaking, FieldRemoved, fmt.Sprintf("%s[%d]", path, i), a[i], nil)
	}
	for i := n; i < len(b); i++ {
		d.add(Additive, FieldAdded, fmt.Sprintf("%s[%d]", path, i), nil, b[i])
	}
}

// walkKeyedArray compares arrays of objects as sets identified by key.
// Elements without the key, or with duplicate keys, are reported as a
// type_changed at the array path because the key is not a usable identity.
func (d *differ) walkKeyedArray(path, key string, a, b []any, depth int) {
	am, aok := indexByKey(a, key)
	bm, bok := indexByKey(b, key)
	if !aok || !bok {
		d.add(Breaking, TypeChanged, path, nil, nil)
		return
	}
	ids := make([]string, 0, len(am)+len(bm))
	for id := range am {
		ids = append(ids, id)
	}
	for id := range bm {
		if _, ok := am[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := fmt.Sprintf("%s[%s=%s]", path, key, id)
		av, inA := am[id]
		bv, inB := bm[id]
		switch {
		case inA && !inB:
			d.add(Breaking, FieldRemoved, p, av, nil)
		case !inA && inB:
			d.add(Additive, FieldAdded, p, nil, bv)
		default:
			d.walk(p, av, bv, depth+1)
		}
	}
}

func indexByKey(items []any, key string) (map[string]any, bool) {
	m := make(map[string]any, len(items))
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			return nil, false
		}
		raw, ok := obj[key]
		if !ok {
			return nil, false
		}
		id := scalarString(raw)
		if id == "" {
			return nil, false
		}
		if _, dup := m[id]; dup {
			return nil, false
		}
		m[id] = obj
	}
	return m, true
}

// walkMultiset compares scalar arrays ignoring order.
func (d *differ) walkMultiset(path string, a, b []any) {
	count := map[string]int{}
	sample := map[string]any{}
	for _, v := range a {
		s := canonical(v)
		count[s]++
		sample[s] = v
	}
	for _, v := range b {
		s := canonical(v)
		count[s]--
		sample[s] = v
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch c := count[k]; {
		case c > 0:
			d.add(Breaking, FieldRemoved, path+"[*]", sample[k], nil)
		case c < 0:
			d.add(Additive, FieldAdded, path+"[*]", nil, sample[k])
		}
	}
}

// ---- helpers ----

func (c Config) valueSeverity() Severity {
	if c.ValueChangeSeverity == "" {
		return Breaking
	}
	return c.ValueChangeSeverity
}

func latencyFinding(base, cand Response, cfg Config) *Finding {
	if cfg.LatencyFactor <= 0 || base.LatencyMS <= 0 {
		return nil
	}
	slower := float64(cand.LatencyMS) > float64(base.LatencyMS)*cfg.LatencyFactor
	if slower && cand.LatencyMS-base.LatencyMS > cfg.LatencyMinMS {
		return &Finding{
			Severity: Latency, Kind: LatencyRegression,
			Baseline: intJSON(base.LatencyMS), Candidate: intJSON(cand.LatencyMS),
		}
	}
	return nil
}

// numbersEqual compares exactly when both parse as integers, otherwise as
// float64 within tol. Exact integer comparison avoids precision loss on ids.
func numbersEqual(a, b json.Number, tol float64) bool {
	if a == b {
		return true
	}
	ai, aerr := strconv.ParseInt(a.String(), 10, 64)
	bi, berr := strconv.ParseInt(b.String(), 10, 64)
	if aerr == nil && berr == nil {
		return ai == bi || (tol > 0 && math.Abs(float64(ai)-float64(bi)) <= tol)
	}
	af, aerr := a.Float64()
	bf, berr := b.Float64()
	if aerr != nil || berr != nil {
		return false
	}
	return math.Abs(af-bf) <= tol || af == bf
}

func childPath(parent, key string) string {
	if isSimpleKey(key) {
		return parent + "." + key
	}
	return parent + "['" + strings.ReplaceAll(key, "'", `\'`) + "']"
}

func isSimpleKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		if !(r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func canonical(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func marshalValue(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	const maxValueBytes = 2048 // keep stored findings small
	if len(b) > maxValueBytes {
		return quote("(" + strconv.Itoa(len(b)) + " bytes, truncated)")
	}
	return b
}

func quote(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func intJSON(n int) json.RawMessage { return json.RawMessage(strconv.Itoa(n)) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortFindings(fs []Finding) {
	rank := map[Severity]int{Breaking: 0, Additive: 1, Latency: 2}
	sort.SliceStable(fs, func(i, j int) bool {
		if rank[fs[i].Severity] != rank[fs[j].Severity] {
			return rank[fs[i].Severity] < rank[fs[j].Severity]
		}
		return fs[i].Path < fs[j].Path
	})
}

// ignored reports whether path matches any IgnorePaths pattern. A pattern
// matches the path itself and everything beneath it; "[*]" in a pattern
// matches any "[...]" index or key segment.
func (d *differ) ignored(path string) bool {
	for _, pat := range d.cfg.IgnorePaths {
		if pathMatches(pat, path) {
			return true
		}
	}
	return false
}

func pathMatches(pattern, path string) bool {
	ps, ss := segments(pattern), segments(path)
	if len(ps) > len(ss) {
		return false
	}
	for i, p := range ps {
		if p == "[*]" {
			if !strings.HasPrefix(ss[i], "[") {
				return false
			}
			continue
		}
		if p != ss[i] {
			return false
		}
	}
	return true
}

// segments splits "$.a.b[0].c" into ["$", ".a", ".b", "[0]", ".c"]. A "." or
// "[" inside a bracketed segment (a quoted key such as ['a.b']) does not split.
func segments(p string) []string {
	var out []string
	start, depth := 0, 0
	for i := 0; i < len(p); i++ {
		switch {
		case p[i] == '[':
			if depth == 0 && i > start {
				out = append(out, p[start:i])
				start = i
			}
			depth++
		case p[i] == ']' && depth > 0:
			depth--
		case p[i] == '.' && depth == 0 && i > start:
			out = append(out, p[start:i])
			start = i
		}
	}
	if start < len(p) {
		out = append(out, p[start:])
	}
	return out
}

// NormalizeIndexed rewrites concrete array selectors in a path to "[*]" so
// noise learned on "$.items[3].ts" or "$.items[id=7].ts" also covers every
// other element. Quoted object keys such as ['a.b'] are left alone.
func NormalizeIndexed(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '[' {
			j := strings.IndexByte(path[i:], ']')
			if j > 0 {
				inner := path[i+1 : i+j]
				_, numErr := strconv.Atoi(inner)
				if numErr == nil || (strings.Contains(inner, "=") && !strings.HasPrefix(inner, "'")) {
					b.WriteString("[*]")
					i += j
					continue
				}
			}
		}
		b.WriteByte(path[i])
	}
	return b.String()
}
