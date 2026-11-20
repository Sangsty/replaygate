package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Sangsty/replaygate/internal/report"
)

const rid = "11111111-1111-1111-1111-111111111111"

// stack is a fake replaygate API plus a fake GitHub, wired to the CLI by env.
type stack struct {
	t        *testing.T
	status   string
	diffs    []report.Diff
	errMsg   string
	mu       sync.Mutex
	comments []string
	postCode int // GitHub's answer to creating a comment; 0 = 201
	env      map[string]string
	dir      string
}

func newStack(t *testing.T, status string) *stack {
	s := &stack{t: t, status: status, dir: t.TempDir()}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1/projects/p1")
		switch {
		case p == "/config":
			_, _ = io.WriteString(w, `{"sanitize_rules":{"headers":[],"query_params":[],"json_paths":[]}}`)
		case p == "/runs" && r.Method == "POST":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"run_id":"`+rid+`","jobs":1}`)
		case p == "/runs/"+rid:
			res := report.Result{Run: report.Run{ID: rid, Status: s.status, TotalRequests: 4, Error: s.errMsg}, Diffs: []report.Diff{}}
			if r.URL.Query().Get("diff_limit") != "0" {
				res.Diffs = s.diffs
				res.Run.Breaking = len(s.diffs)
			}
			_ = json.NewEncoder(w).Encode(res)
		default:
			w.WriteHeader(http.StatusNoContent) // job claims: nothing to do
		}
	}))
	t.Cleanup(api.Close)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == "GET":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST":
			if s.postCode != 0 {
				w.WriteHeader(s.postCode)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
				return
			}
			var in struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			s.comments = append(s.comments, in.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":1,"html_url":"https://github.example/c/1"}`)
		}
	}))
	t.Cleanup(gh.Close)

	s.env = map[string]string{
		"RG_API_URL": api.URL, "RG_PROJECT_ID": "p1", "RG_API_KEY": "rg_secretkey",
		"RG_BASELINE_URL": "http://127.0.0.1:1", "RG_CANDIDATE_URL": "http://127.0.0.1:1",
		"RG_COMMIT_SHA": "abcdef1", "RG_PR_NUMBER": "9",
		"GITHUB_TOKEN": "ghs_secrettoken", "GITHUB_REPOSITORY": "o/r", "GITHUB_API_URL": gh.URL,
		"GITHUB_STEP_SUMMARY": filepath.Join(s.dir, "summary.md"),
		"GITHUB_OUTPUT":       filepath.Join(s.dir, "output.txt"),
		"RG_DASHBOARD_URL":    "https://rg.example/",
		"RG_TIMEOUT":          "10s",
	}
	return s
}

func (s *stack) run() (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(context.Background(), []string{"run"}, func(k string) string { return s.env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestFailedCheck(t *testing.T) {
	s := newStack(t, "failed")
	s.diffs = []report.Diff{{RequestID: 1, Endpoint: "/u/{id}", Severity: "breaking", Kind: "field_removed", JSONPath: "$.name"}}
	code, stdout, stderr := s.run()
	if code != exitFailed {
		t.Fatalf("exit = %d, want %d\n%s", code, exitFailed, stderr)
	}
	if !strings.Contains(stdout, "FAILED") || !strings.Contains(stdout, "$.name") {
		t.Errorf("stdout = %s", stdout)
	}
	if len(s.comments) != 1 || !strings.Contains(s.comments[0], report.Marker) ||
		!strings.Contains(s.comments[0], "https://rg.example/p/p1/runs/"+rid) {
		t.Errorf("comment = %v", s.comments)
	}
	if got := readFile(t, s.env["GITHUB_STEP_SUMMARY"]); !strings.Contains(got, "Replay Gate failed") {
		t.Errorf("step summary = %s", got)
	}
	out := readFile(t, s.env["GITHUB_OUTPUT"])
	for _, want := range []string{"verdict=failed", "run-id=" + rid, "breaking-count=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("outputs missing %q: %s", want, out)
		}
	}
}

func TestPassedCheck(t *testing.T) {
	s := newStack(t, "passed")
	if code, _, stderr := s.run(); code != exitPassed {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	if len(s.comments) != 1 || !strings.Contains(s.comments[0], "✅") {
		t.Errorf("comment = %v", s.comments)
	}
}

func TestErroredRunIsNotAPass(t *testing.T) {
	s := newStack(t, "errored")
	s.errMsg = "baseline looks broken"
	code, stdout, _ := s.run()
	if code != exitErrored {
		t.Fatalf("exit = %d, want %d", code, exitErrored)
	}
	if !strings.Contains(stdout, "baseline looks broken") || !strings.Contains(s.comments[0], "could not finish") {
		t.Errorf("stdout=%s comments=%v", stdout, s.comments)
	}
}

// The check result must not depend on whether GitHub accepts the comment.
func TestCommentFailureDoesNotChangeTheVerdict(t *testing.T) {
	s := newStack(t, "passed")
	s.postCode = http.StatusForbidden
	code, _, stderr := s.run()
	if code != exitPassed {
		t.Fatalf("exit = %d, want %d", code, exitPassed)
	}
	if !strings.Contains(stderr, "could not post the pull request comment") {
		t.Errorf("missing warning: %s", stderr)
	}
	if strings.Contains(stderr, "ghs_secrettoken") || strings.Contains(stderr, "rg_secretkey") {
		t.Errorf("a credential leaked into the log:\n%s", stderr)
	}
}

func TestNoCommentWhenNotApplicable(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"not a pull request": func(e map[string]string) { delete(e, "RG_PR_NUMBER") },
		"no token":           func(e map[string]string) { delete(e, "GITHUB_TOKEN") },
		"disabled":           func(e map[string]string) { e["RG_COMMENT"] = "false" },
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t, "passed")
			mutate(s.env)
			if code, _, stderr := s.run(); code != exitPassed {
				t.Fatalf("exit = %d\n%s", code, stderr)
			}
			if len(s.comments) != 0 {
				t.Errorf("a comment was posted: %v", s.comments)
			}
		})
	}
}

func TestReportFile(t *testing.T) {
	s := newStack(t, "passed")
	s.env["RG_REPORT_FILE"] = filepath.Join(s.dir, "report.md")
	s.run()
	if got := readFile(t, s.env["RG_REPORT_FILE"]); !strings.Contains(got, report.Marker) {
		t.Errorf("report file = %s", got)
	}
}

func TestConfigurationErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"missing key", func(e map[string]string) { delete(e, "RG_API_KEY") }, "RG_API_KEY"},
		{"missing candidate", func(e map[string]string) { delete(e, "RG_CANDIDATE_URL") }, "RG_CANDIDATE_URL"},
		{"relative url", func(e map[string]string) { e["RG_BASELINE_URL"] = "localhost:3000" }, "RG_BASELINE_URL"},
		{"bad sample size", func(e map[string]string) { e["RG_SAMPLE_SIZE"] = "0" }, "RG_SAMPLE_SIZE"},
		{"bad bool", func(e map[string]string) { e["RG_NOISE_LEARNING"] = "maybe" }, "RG_NOISE_LEARNING"},
		{"bad duration", func(e map[string]string) { e["RG_TIMEOUT"] = "soon" }, "RG_TIMEOUT"},
		{"bad dashboard url", func(e map[string]string) { e["RG_DASHBOARD_URL"] = "javascript:alert(1)" }, "RG_DASHBOARD_URL"},
		{"no commit", func(e map[string]string) { delete(e, "RG_COMMIT_SHA") }, "commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStack(t, "passed")
			tt.mutate(s.env)
			code, _, stderr := s.run()
			if code != exitErrored || !strings.Contains(stderr, tt.want) {
				t.Errorf("exit = %d, stderr = %s", code, stderr)
			}
		})
	}
}

func TestUnknownCommand(t *testing.T) {
	var errb bytes.Buffer
	if code := run(context.Background(), []string{"frobnicate"}, func(string) string { return "" }, io.Discard, &errb); code != exitErrored ||
		!strings.Contains(errb.String(), "usage") {
		t.Errorf("code = %d, stderr = %s", code, errb.String())
	}
}

func TestInfrastructureFailureExitsTwo(t *testing.T) {
	s := newStack(t, "passed")
	s.env["RG_API_URL"] = "http://127.0.0.1:1" // nothing listens
	s.env["GITHUB_OUTPUT"] = filepath.Join(s.dir, "out.txt")
	code, _, stderr := s.run()
	if code != exitErrored || !strings.Contains(stderr, "could not be completed") {
		t.Errorf("exit = %d, stderr = %s", code, stderr)
	}
	if got := readFile(t, s.env["GITHUB_OUTPUT"]); !strings.Contains(got, "verdict=errored") {
		t.Errorf("outputs = %s", got)
	}
}
