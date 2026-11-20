// Package report turns a finished run into text people read: a Markdown
// comment for the pull request and a plain-text summary for the CI log.
//
// The types mirror the JSON the API returns for GET .../runs/{id}. They live
// here, not in the store package, so the CLI does not link the database driver.
package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Marker identifies the comment this tool owns, so a re-run edits it instead of
// adding a new one. It is invisible when the comment is rendered.
const Marker = "<!-- replaygate:report -->"

// Severities and statuses, as the API spells them.
const (
	SevBreaking = "breaking"
	SevAdditive = "additive"
	SevLatency  = "latency"

	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusErrored = "errored"
)

// Run is the run summary.
type Run struct {
	ID            string     `json:"id"`
	CommitSHA     string     `json:"commit_sha"`
	PRNumber      *int       `json:"pr_number,omitempty"`
	Status        string     `json:"status"`
	TotalRequests int        `json:"total_requests"`
	Breaking      int        `json:"breaking_count"`
	Additive      int        `json:"additive_count"`
	Latency       int        `json:"latency_count"`
	Error         string     `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// Terminal reports whether the run will not change any more.
func (r Run) Terminal() bool {
	return r.Status == StatusPassed || r.Status == StatusFailed || r.Status == StatusErrored
}

// Diff is one finding.
type Diff struct {
	RequestID int64           `json:"request_id"`
	Endpoint  string          `json:"endpoint"`
	Severity  string          `json:"severity"`
	Kind      string          `json:"kind"`
	JSONPath  string          `json:"json_path"`
	Baseline  json.RawMessage `json:"baseline_value,omitempty"`
	Candidate json.RawMessage `json:"candidate_value,omitempty"`
}

// NoiseField is a field the run learned to ignore because it differed between
// two replays of the same baseline.
type NoiseField struct {
	Endpoint string `json:"endpoint"`
	JSONPath string `json:"json_path"`
}

// Result is what GET .../runs/{id} returns.
type Result struct {
	Run   Run          `json:"run"`
	Diffs []Diff       `json:"diffs"`
	Noise []NoiseField `json:"noise"`
}

// Options tunes rendering.
type Options struct {
	// RunURL links to the run in the dashboard. Empty omits the link.
	RunURL string
	// RowsPerSection caps the table rows in each severity section (default 25).
	RowsPerSection int
	// MaxBytes caps the whole comment (default 60000; GitHub's limit is 65536).
	MaxBytes int
}

const (
	defaultRows     = 25
	defaultMaxBytes = 60000
	maxNoiseListed  = 50
	maxCell         = 60
	maxPathCell     = 100
)

var kindLabel = map[string]string{
	"status_changed":     "status changed",
	"field_removed":      "field removed",
	"type_changed":       "type changed",
	"value_changed":      "value changed",
	"field_added":        "field added",
	"latency_regression": "slower",
	"request_failed":     "request failed",
}

// group is a set of findings that share an endpoint, kind and path: the same
// problem seen on several requests.
type group struct {
	Diff  // the first example
	Count int
}

func groupDiffs(diffs []Diff, severity string) []group {
	var out []group
	idx := map[string]int{}
	for _, d := range diffs {
		if d.Severity != severity {
			continue
		}
		k := d.Endpoint + "\x00" + d.Kind + "\x00" + d.JSONPath
		if i, ok := idx[k]; ok {
			out[i].Count++
			continue
		}
		idx[k] = len(out)
		out = append(out, group{Diff: d, Count: 1})
	}
	return out
}

// Markdown renders the pull request comment. Every value that came from the
// application under test goes through code(), so a response body cannot inject
// Markdown, break a table or mention a user.
func Markdown(res Result, opt Options) string {
	rows, maxBytes := opt.RowsPerSection, opt.MaxBytes
	if rows <= 0 {
		rows = defaultRows
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	r := res.Run

	var b strings.Builder
	b.WriteString(Marker + "\n")
	switch r.Status {
	case StatusPassed:
		b.WriteString("## ✅ Replay Gate passed\n\n")
	case StatusFailed:
		b.WriteString("## ❌ Replay Gate failed\n\n")
	case StatusErrored:
		b.WriteString("## ⚠️ Replay Gate could not finish\n\n")
	default:
		b.WriteString("## ⏳ Replay Gate is still running\n\n")
	}

	if r.Status == StatusErrored {
		b.WriteString("The run stopped before it could compare the builds, so this check says nothing about your change.\n\n")
		if r.Error != "" {
			b.WriteString("> " + oneLine(r.Error, 300) + "\n\n")
		}
	} else {
		fmt.Fprintf(&b, "Replayed **%d** recorded requests against the baseline and this PR: **%d breaking**, %d additive, %d latency.\n\n",
			r.TotalRequests, r.Breaking, r.Additive, r.Latency)
	}

	for _, sec := range []struct {
		sev, title string
		open       bool
	}{
		{SevBreaking, "Breaking changes", true},
		{SevAdditive, "Additive changes", false},
		{SevLatency, "Latency regressions", false},
	} {
		groups := groupDiffs(res.Diffs, sec.sev)
		if len(groups) == 0 {
			continue
		}
		b.WriteString(section(sec.title, sec.open, groups, rows))
	}

	if shown, total := len(res.Diffs), r.Breaking+r.Additive+r.Latency; total > shown {
		fmt.Fprintf(&b, "_Showing %d of %d findings; the rest are in the dashboard._\n\n", shown, total)
	}
	if len(res.Noise) > 0 {
		b.WriteString(noiseSection(res.Noise))
	}

	b.WriteString("<sub>")
	fmt.Fprintf(&b, "commit %s · run %s", code(shortSHA(r.CommitSHA)), code(shortSHA(r.ID)))
	if opt.RunURL != "" {
		fmt.Fprintf(&b, " · [open in dashboard](%s)", opt.RunURL)
	}
	b.WriteString("</sub>\n")

	out := b.String()
	if len(out) > maxBytes {
		cut := out[:maxBytes]
		for !utf8.ValidString(cut) { // do not cut a character in half
			cut = cut[:len(cut)-1]
		}
		out = cut + "\n\n_Report truncated. Open the run in the dashboard for the rest._\n"
	}
	return out
}

func section(title string, open bool, groups []group, rows int) string {
	var b strings.Builder
	n := 0
	for _, g := range groups {
		n += g.Count
	}
	attr := ""
	if open {
		attr = " open"
	}
	fmt.Fprintf(&b, "<details%s><summary><b>%s (%d)</b></summary>\n\n", attr, title, n)
	b.WriteString("| Endpoint | Change | Path | Baseline → PR | Requests |\n|---|---|---|---|---|\n")
	for i, g := range groups {
		if i == rows {
			break
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s → %s | %d |\n",
			code(clip(g.Endpoint, maxPathCell)), label(g.Kind), code(clip(pathOrDash(g.JSONPath), maxPathCell)),
			value(g.Baseline), value(g.Candidate), g.Count)
	}
	if len(groups) > rows {
		fmt.Fprintf(&b, "\n_…and %d more kinds of change._\n", len(groups)-rows)
	}
	b.WriteString("\n</details>\n\n")
	return b.String()
}

func noiseSection(noise []NoiseField) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<details><summary>Ignored as noise (%d fields)</summary>\n\nThese fields differed between two replays of the baseline, so they were not compared.\n\n", len(noise))
	for i, n := range noise {
		if i == maxNoiseListed {
			fmt.Fprintf(&b, "- …and %d more\n", len(noise)-maxNoiseListed)
			break
		}
		fmt.Fprintf(&b, "- %s %s\n", code(clip(n.Endpoint, maxPathCell)), code(clip(n.JSONPath, maxPathCell)))
	}
	b.WriteString("\n</details>\n\n")
	return b.String()
}

// Text renders a compact summary for the CI log.
func Text(res Result) string {
	r := res.Run
	var b strings.Builder
	fmt.Fprintf(&b, "Replay Gate: %s | requests=%d breaking=%d additive=%d latency=%d\n",
		strings.ToUpper(r.Status), r.TotalRequests, r.Breaking, r.Additive, r.Latency)
	if r.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", oneLine(r.Error, 300))
	}
	for _, sev := range []string{SevBreaking, SevAdditive, SevLatency} {
		for _, g := range groupDiffs(res.Diffs, sev) {
			fmt.Fprintf(&b, "  %-8s %-14s %-24s %-20s %s -> %s  (x%d)\n", sev, g.Kind,
				clip(g.Endpoint, 24), clip(pathOrDash(g.JSONPath), 20), plain(g.Baseline), plain(g.Candidate), g.Count)
		}
	}
	if len(res.Noise) > 0 {
		fmt.Fprintf(&b, "ignored as noise: %d fields\n", len(res.Noise))
	}
	return b.String()
}

func label(kind string) string {
	if l, ok := kindLabel[kind]; ok {
		return l
	}
	return code(kind)
}

func pathOrDash(p string) string {
	if p == "" {
		return "(response)"
	}
	return p
}

// value renders a JSON value for a table cell. An absent value is a dash.
func value(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "—"
	}
	return code(clip(compact(raw), maxCell))
}

func plain(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "-"
	}
	return clip(compact(raw), maxCell)
}

func compact(raw json.RawMessage) string {
	return oneLine(string(raw), 1<<12)
}

// code wraps s in a Markdown code span that cannot be broken out of and that
// is safe inside a table cell.
func code(s string) string {
	s = oneLine(s, 1<<12)
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "|", `\|`)
	if s == "" {
		return "—"
	}
	return "`" + s + "`"
}

// oneLine collapses whitespace and control characters into single spaces.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, max)
}

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
