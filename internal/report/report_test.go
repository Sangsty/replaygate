package report

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func sample() Result {
	return Result{
		Run: Run{ID: "11111111-2222-3333-4444-555555555555", CommitSHA: "abcdef0123456789", Status: StatusFailed,
			TotalRequests: 11, Breaking: 2, Additive: 2},
		Diffs: []Diff{
			{RequestID: 1, Endpoint: "/orders/{id}", Severity: SevBreaking, Kind: "value_changed", JSONPath: "$.total", Baseline: raw(`20.0`), Candidate: raw(`99.0`)},
			{RequestID: 2, Endpoint: "/users/{id}", Severity: SevBreaking, Kind: "field_removed", JSONPath: "$.name", Baseline: raw(`"user 3"`)},
			{RequestID: 3, Endpoint: "/orders/{id}", Severity: SevAdditive, Kind: "field_added", JSONPath: "$.currency", Candidate: raw(`"USD"`)},
			{RequestID: 4, Endpoint: "/orders/{id}", Severity: SevAdditive, Kind: "field_added", JSONPath: "$.currency", Candidate: raw(`"USD"`)},
		},
		Noise: []NoiseField{{Endpoint: "/users/{id}", JSONPath: "$.ts"}},
	}
}

func TestMarkdownFailedRun(t *testing.T) {
	md := Markdown(sample(), Options{RunURL: "https://rg.example/p/1/runs/2"})
	for _, want := range []string{
		Marker,
		"❌ Replay Gate failed",
		"**11** recorded requests",
		"**2 breaking**, 2 additive",
		"<details open><summary><b>Breaking changes (2)</b>",
		"| `/orders/{id}` | value changed | `$.total` | `20.0` → `99.0` | 1 |",
		"| `/users/{id}` | field removed | `$.name` | `\"user 3\"` → — | 1 |",
		"Additive changes (2)", // two findings, one group
		"| `/orders/{id}` | field added | `$.currency` | — → `\"USD\"` | 2 |",
		"Ignored as noise (1 fields)",
		"[open in dashboard](https://rg.example/p/1/runs/2)",
		"commit `abcdef01`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown is missing %q\n%s", want, md)
		}
	}
	if strings.Contains(md, "Latency regressions") {
		t.Error("an empty section must not be rendered")
	}
}

func TestMarkdownStates(t *testing.T) {
	tests := []struct {
		name   string
		run    Run
		want   string
		absent string
	}{
		{"passed", Run{Status: StatusPassed, TotalRequests: 5}, "✅ Replay Gate passed", "Breaking changes"},
		{"errored", Run{Status: StatusErrored, Error: "baseline looks broken:\n7 of 9 failed"}, "⚠️ Replay Gate could not finish", "Replayed"},
		{"running", Run{Status: "replaying"}, "⏳ Replay Gate is still running", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := Markdown(Result{Run: tt.run}, Options{})
			if !strings.Contains(md, tt.want) {
				t.Errorf("missing %q in\n%s", tt.want, md)
			}
			if tt.absent != "" && strings.Contains(md, tt.absent) {
				t.Errorf("unexpected %q in\n%s", tt.absent, md)
			}
		})
	}
	md := Markdown(Result{Run: Run{Status: StatusErrored, Error: "line one\nline two"}}, Options{})
	if !strings.Contains(md, "> line one line two") {
		t.Errorf("error text must be one quoted line:\n%s", md)
	}
}

// Response data is attacker-influenced (a PR can change what the app returns).
// It must never break out of its table cell or notify anyone.
func TestMarkdownNeutralisesHostileValues(t *testing.T) {
	res := Result{
		Run: Run{Status: StatusFailed, Breaking: 1},
		Diffs: []Diff{{
			Endpoint: "/x|y", Severity: SevBreaking, Kind: "value_changed", JSONPath: "$.a`b",
			Baseline:  raw("\"line1\\nline2 | @octocat `code` <img src=x>\""),
			Candidate: raw(`"ok"`),
		}},
	}
	md := Markdown(res, Options{})
	row := ""
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "| `/x") {
			row = l
		}
	}
	if row == "" {
		t.Fatalf("row not found:\n%s", md)
	}
	// 5 columns means 6 unescaped pipes; escaped pipes are inside code spans.
	if n := strings.Count(strings.ReplaceAll(row, `\|`, ""), "|"); n != 6 {
		t.Errorf("row has %d unescaped pipes, want 6: %s", n, row)
	}
	if strings.Contains(row, "\n") || strings.Count(row, "`")%2 != 0 {
		t.Errorf("row has an unbalanced code span: %s", row)
	}
	if !strings.Contains(row, "`") || strings.Contains(row, "` @octocat") {
		t.Errorf("@mention must stay inside a code span: %s", row)
	}
}

func TestMarkdownCapsRowsAndSize(t *testing.T) {
	var diffs []Diff
	for i := 0; i < 100; i++ {
		diffs = append(diffs, Diff{RequestID: int64(i), Endpoint: "/e", Severity: SevBreaking, Kind: "value_changed",
			JSONPath: "$.f" + strings.Repeat("x", i), Baseline: raw(`1`), Candidate: raw(`2`)})
	}
	res := Result{Run: Run{Status: StatusFailed, Breaking: 100}, Diffs: diffs}
	md := Markdown(res, Options{RowsPerSection: 10})
	if got := strings.Count(md, "| `/e` |"); got != 10 {
		t.Errorf("rows = %d, want 10", got)
	}
	if !strings.Contains(md, "and 90 more kinds of change") {
		t.Error("missing the overflow note")
	}

	small := Markdown(res, Options{RowsPerSection: 100, MaxBytes: 500})
	if len(small) > 500+len("\n\n_Report truncated. Open the run in the dashboard for the rest._\n") || !strings.Contains(small, "Report truncated") {
		t.Errorf("size cap not applied: %d bytes", len(small))
	}
	if !utf8.ValidString(small) {
		t.Error("truncation split a character")
	}
}

func TestMarkdownNotesHiddenFindings(t *testing.T) {
	res := sample()
	res.Run.Breaking = 50 // the API returned fewer diffs than the run counted
	md := Markdown(res, Options{})
	if !strings.Contains(md, "Showing 4 of 52 findings") {
		t.Errorf("missing partial-results note:\n%s", md)
	}
}

func TestText(t *testing.T) {
	txt := Text(sample())
	for _, want := range []string{"FAILED | requests=11 breaking=2 additive=2", "value_changed", "$.total", "20.0 -> 99.0", "(x2)", "ignored as noise: 1 fields"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text is missing %q\n%s", want, txt)
		}
	}
}

func TestTerminal(t *testing.T) {
	for st, want := range map[string]bool{"passed": true, "failed": true, "errored": true, "pending": false, "replaying": false, "diffing": false} {
		if got := (Run{Status: st}).Terminal(); got != want {
			t.Errorf("Terminal(%s) = %v", st, got)
		}
	}
}

func TestClipAndOneLine(t *testing.T) {
	if got := clip("héllo wörld", 5); got != "héllo…" {
		t.Errorf("clip = %q", got)
	}
	if got := oneLine("a\tb\r\nc d", 100); got != "a b c d" {
		t.Errorf("oneLine = %q", got)
	}
	if got := code(""); got != "—" {
		t.Errorf("code(\"\") = %q", got)
	}
}
