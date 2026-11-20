package diff

import (
	"fmt"
	"strings"
	"testing"
)

func resp(body string) Response { return Response{Status: 200, Body: []byte(body), LatencyMS: 10} }

// summary renders findings as "severity kind path" for compact assertions.
func summary(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = fmt.Sprintf("%s %s %s", f.Severity, f.Kind, f.Path)
	}
	return out
}

func assertFindings(t *testing.T, got []Finding, want []string) {
	t.Helper()
	g := summary(got)
	if len(g) != len(want) {
		t.Fatalf("findings = %q, want %q", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Errorf("finding %d = %q, want %q", i, g[i], want[i])
		}
	}
}

func TestCompareBodies(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		cfg  Config
		want []string
	}{
		{"identical", `{"a":1,"b":"x"}`, `{"b":"x","a":1}`, Config{}, nil},
		{"whitespace and key order ignored", "{ \"a\" : 1 ,\n \"b\":[1,2] }", `{"b":[1,2],"a":1}`, Config{}, nil},
		{"value changed", `{"a":1}`, `{"a":2}`, Config{}, []string{"breaking value_changed $.a"}},
		{"string value changed", `{"n":"x"}`, `{"n":"y"}`, Config{}, []string{"breaking value_changed $.n"}},
		{"bool changed", `{"ok":true}`, `{"ok":false}`, Config{}, []string{"breaking value_changed $.ok"}},
		{"field removed", `{"a":1,"b":2}`, `{"a":1}`, Config{}, []string{"breaking field_removed $.b"}},
		{"field added", `{"a":1}`, `{"a":1,"b":2}`, Config{}, []string{"additive field_added $.b"}},
		{"type changed number to string", `{"a":1}`, `{"a":"1"}`, Config{}, []string{"breaking type_changed $.a"}},
		{"type changed to null", `{"a":1}`, `{"a":null}`, Config{}, []string{"breaking type_changed $.a"}},
		{"null to value", `{"a":null}`, `{"a":1}`, Config{}, []string{"breaking type_changed $.a"}},
		{"object to array", `{"a":{}}`, `{"a":[]}`, Config{}, []string{"breaking type_changed $.a"}},
		{"nested path", `{"u":{"addr":{"zip":"1"}}}`, `{"u":{"addr":{"zip":"2"}}}`, Config{}, []string{"breaking value_changed $.u.addr.zip"}},
		{"key needing quotes", `{"a.b":1}`, `{"a.b":2}`, Config{}, []string{"breaking value_changed $['a.b']"}},
		{"root array element changed", `[1,2,3]`, `[1,9,3]`, Config{}, []string{"breaking value_changed $[1]"}},
		{"array shrank", `[1,2,3]`, `[1,2]`, Config{}, []string{"breaking field_removed $[2]"}},
		{"array grew", `[1,2]`, `[1,2,3]`, Config{}, []string{"additive field_added $[2]"}},
		{"array reorder is a change in order mode", `[1,2]`, `[2,1]`, Config{},
			[]string{"breaking value_changed $[0]", "breaking value_changed $[1]"}},
		{"value change downgraded to additive", `{"a":1}`, `{"a":2}`, Config{ValueChangeSeverity: Additive}, []string{"additive value_changed $.a"}},
		{"empty bodies equal", ``, `  `, Config{}, nil},
		{"non-JSON equal", `plain`, `plain`, Config{}, nil},
		{"non-JSON different", `plain`, `other`, Config{}, []string{"breaking value_changed $"}},
		{"json vs non-json", `{"a":1}`, `<html>`, Config{}, []string{"breaking type_changed $"}},
		{"trailing garbage is not json", `{"a":1} x`, `{"a":1}`, Config{}, []string{"breaking type_changed $"}},
		{"large int exact", `{"id":9007199254740993}`, `{"id":9007199254740992}`, Config{}, []string{"breaking value_changed $.id"}},
		{"1.0 equals 1.00 numerically", `{"p":1.0}`, `{"p":1.00}`, Config{}, nil},
		{"0.1 vs 0.10000000000000001 equal as float64", `{"p":0.1}`, `{"p":0.10000000000000001}`, Config{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, Compare(resp(tt.a), resp(tt.b), tt.cfg), tt.want)
		})
	}
}

func TestFloatTolerance(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		tol  float64
		want []string
	}{
		{"within tolerance", `{"p":1.0000}`, `{"p":1.0004}`, 0.001, nil},
		{"outside tolerance", `{"p":1.0}`, `{"p":1.01}`, 0.001, []string{"breaking value_changed $.p"}},
		{"exactly at tolerance", `{"p":1.0}`, `{"p":1.5}`, 0.5, nil},
		{"1.0 vs 1.00 with tiny tolerance", `{"p":1.0}`, `{"p":1.00}`, 1e-9, nil},
		{"integers with tolerance", `{"n":100}`, `{"n":101}`, 1, nil},
		{"integers beyond tolerance", `{"n":100}`, `{"n":103}`, 1, []string{"breaking value_changed $.n"}},
		{"scientific notation", `{"p":1e2}`, `{"p":100}`, 1e-9, nil},
		{"negative numbers", `{"p":-0.5}`, `{"p":-0.5004}`, 0.001, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, Compare(resp(tt.a), resp(tt.b), Config{FloatTolerance: tt.tol}), tt.want)
		})
	}
}

func TestKeyedArrays(t *testing.T) {
	cfg := Config{ArrayKeys: map[string]string{"$.items": "id"}}
	tests := []struct {
		name string
		a, b string
		want []string
	}{
		{"reordered but equal",
			`{"items":[{"id":1,"v":"a"},{"id":2,"v":"b"}]}`,
			`{"items":[{"id":2,"v":"b"},{"id":1,"v":"a"}]}`, nil},
		{"element value changed",
			`{"items":[{"id":1,"v":"a"},{"id":2,"v":"b"}]}`,
			`{"items":[{"id":2,"v":"B"},{"id":1,"v":"a"}]}`,
			[]string{"breaking value_changed $.items[id=2].v"}},
		{"element removed",
			`{"items":[{"id":1},{"id":2}]}`,
			`{"items":[{"id":1}]}`,
			[]string{"breaking field_removed $.items[id=2]"}},
		{"element added",
			`{"items":[{"id":1}]}`,
			`{"items":[{"id":1},{"id":2}]}`,
			[]string{"additive field_added $.items[id=2]"}},
		{"string keys",
			`{"items":[{"id":"x","v":1}]}`,
			`{"items":[{"id":"x","v":2}]}`,
			[]string{"breaking value_changed $.items[id=x].v"}},
		{"duplicate keys cannot be keyed",
			`{"items":[{"id":1},{"id":1}]}`,
			`{"items":[{"id":1},{"id":1}]}`,
			[]string{"breaking type_changed $.items"}},
		{"missing key field",
			`{"items":[{"name":"a"}]}`,
			`{"items":[{"name":"a"}]}`,
			[]string{"breaking type_changed $.items"}},
		{"empty arrays", `{"items":[]}`, `{"items":[]}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, Compare(resp(tt.a), resp(tt.b), cfg), tt.want)
		})
	}
}

func TestUnorderedScalarArrays(t *testing.T) {
	cfg := Config{UnorderedArrays: []string{"$.tags"}}
	tests := []struct {
		name string
		a, b string
		want []string
	}{
		{"reordered", `{"tags":["a","b","c"]}`, `{"tags":["c","a","b"]}`, nil},
		{"one removed", `{"tags":["a","b"]}`, `{"tags":["a"]}`, []string{"breaking field_removed $.tags[*]"}},
		{"one added", `{"tags":["a"]}`, `{"tags":["a","b"]}`, []string{"additive field_added $.tags[*]"}},
		{"duplicate count matters", `{"tags":["a","a"]}`, `{"tags":["a"]}`, []string{"breaking field_removed $.tags[*]"}},
		{"numbers", `{"tags":[1,2]}`, `{"tags":[2,1]}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, Compare(resp(tt.a), resp(tt.b), cfg), tt.want)
		})
	}
}

func TestIgnorePaths(t *testing.T) {
	tests := []struct {
		name   string
		a, b   string
		ignore []string
		want   []string
	}{
		{"exact field ignored", `{"ts":1,"a":1}`, `{"ts":2,"a":1}`, []string{"$.ts"}, nil},
		{"other fields still diffed", `{"ts":1,"a":1}`, `{"ts":2,"a":2}`, []string{"$.ts"}, []string{"breaking value_changed $.a"}},
		{"subtree ignored", `{"meta":{"x":1,"y":2}}`, `{"meta":{"x":9,"y":8}}`, []string{"$.meta"}, nil},
		{"removed field ignored", `{"ts":1,"a":1}`, `{"a":1}`, []string{"$.ts"}, nil},
		{"added field ignored", `{"a":1}`, `{"a":1,"ts":5}`, []string{"$.ts"}, nil},
		{"wildcard index", `{"i":[{"ts":1},{"ts":2}]}`, `{"i":[{"ts":8},{"ts":9}]}`, []string{"$.i[*].ts"}, nil},
		{"wildcard does not match object key", `{"i":{"k":{"ts":1}}}`, `{"i":{"k":{"ts":2}}}`, []string{"$.i[*].ts"},
			[]string{"breaking value_changed $.i.k.ts"}},
		{"prefix of a different key is not matched", `{"tsx":1}`, `{"tsx":2}`, []string{"$.ts"}, []string{"breaking value_changed $.tsx"}},
		{"quoted key", `{"a.b":1}`, `{"a.b":2}`, []string{"$['a.b']"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, Compare(resp(tt.a), resp(tt.b), Config{IgnorePaths: tt.ignore}), tt.want)
		})
	}
}

func TestStatusAndFailures(t *testing.T) {
	tests := []struct {
		name       string
		base, cand Response
		want       []string
	}{
		{"status changed skips body diff",
			Response{Status: 200, Body: []byte(`{"a":1}`)}, Response{Status: 500, Body: []byte(`oops`)},
			[]string{"breaking status_changed "}},
		{"same error status compares bodies",
			Response{Status: 404, Body: []byte(`{"e":"a"}`)}, Response{Status: 404, Body: []byte(`{"e":"b"}`)},
			[]string{"breaking value_changed $.e"}},
		{"candidate request failed",
			Response{Status: 200}, Response{Err: "timeout"},
			[]string{"breaking request_failed "}},
		{"both failed", Response{Err: "refused"}, Response{Err: "refused"}, nil},
		{"baseline failed, candidate ok", Response{Err: "refused"}, Response{Status: 200}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, Compare(tt.base, tt.cand, Config{}), tt.want)
		})
	}
}

func TestLatency(t *testing.T) {
	cfg := Config{LatencyFactor: 2, LatencyMinMS: 50}
	tests := []struct {
		name         string
		base, cand   int
		cfg          Config
		wantFindings int
	}{
		{"slower and above floor", 100, 300, cfg, 1},
		{"slower factor but below absolute floor", 10, 40, cfg, 0},
		{"within factor", 100, 150, cfg, 0},
		{"exactly at factor is not a regression", 100, 200, cfg, 0},
		{"disabled by default", 100, 5000, Config{}, 0},
		{"no baseline latency", 0, 5000, cfg, 0},
		{"faster", 300, 100, cfg, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compare(
				Response{Status: 200, Body: []byte(`{}`), LatencyMS: tt.base},
				Response{Status: 200, Body: []byte(`{}`), LatencyMS: tt.cand}, tt.cfg)
			if len(got) != tt.wantFindings {
				t.Fatalf("findings = %q, want %d", summary(got), tt.wantFindings)
			}
			if tt.wantFindings == 1 && (got[0].Severity != Latency || got[0].Kind != LatencyRegression) {
				t.Errorf("finding = %+v", got[0])
			}
		})
	}
}

func TestFindingsAreSortedBySeverityThenPath(t *testing.T) {
	cfg := Config{LatencyFactor: 2, LatencyMinMS: 1}
	got := Compare(
		Response{Status: 200, Body: []byte(`{"z":1,"b":1,"gone":1}`), LatencyMS: 10},
		Response{Status: 200, Body: []byte(`{"z":2,"b":2,"new":1}`), LatencyMS: 100}, cfg)
	assertFindings(t, got, []string{
		"breaking value_changed $.b",
		"breaking field_removed $.gone",
		"breaking value_changed $.z",
		"additive field_added $.new",
		"latency latency_regression ",
	})
}

func TestFindingValuesAreRecorded(t *testing.T) {
	got := Compare(resp(`{"a":1}`), resp(`{"a":2}`), Config{})
	if len(got) != 1 || string(got[0].Baseline) != "1" || string(got[0].Candidate) != "2" {
		t.Fatalf("got %+v, want baseline 1 and candidate 2", got)
	}
}

func TestHugeValuesAreTruncatedInFindings(t *testing.T) {
	big := `"` + strings.Repeat("x", 10000) + `"`
	got := Compare(resp(`{"a":`+big+`}`), resp(`{"a":"y"}`), Config{})
	if len(got) != 1 || len(got[0].Baseline) > 200 {
		t.Fatalf("baseline value not truncated: %d bytes", len(got[0].Baseline))
	}
}

func TestDeepNestingDoesNotOverflow(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 5000) + `1` + strings.Repeat(`}`, 5000)
	other := strings.Repeat(`{"a":`, 5000) + `2` + strings.Repeat(`}`, 5000)
	got := Compare(resp(deep), resp(other), Config{})
	if len(got) == 0 {
		t.Fatal("expected a finding for deeply nested difference")
	}
}

func TestNormalizeIndexed(t *testing.T) {
	tests := []struct{ in, want string }{
		{"$.items[3].ts", "$.items[*].ts"},
		{"$.a[0][12].b", "$.a[*][*].b"},
		{"$.items[id=5].ts", "$.items[*].ts"},
		{"$['a.b'].c", "$['a.b'].c"},
		{"$", "$"},
	}
	for _, tt := range tests {
		if got := NormalizeIndexed(tt.in); got != tt.want {
			t.Errorf("NormalizeIndexed(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPathMatches(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"$.a", "$.a", true},
		{"$.a", "$.a.b", true},
		{"$.a", "$.ab", false},
		{"$.a.b", "$.a", false},
		{"$.a[*].b", "$.a[4].b", true},
		{"$.a[*].b", "$.a[id=7].b", true},
		{"$.a[*].b", "$.a.x.b", false},
		{"$['x.y']", "$['x.y'].z", true},
	}
	for _, tt := range tests {
		if got := pathMatches(tt.pattern, tt.path); got != tt.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}
