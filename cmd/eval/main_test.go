package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWilsonInterval(t *testing.T) {
	cases := []struct {
		k, n         int
		wantLo, want float64 // bounds from the Wilson score formula
	}{
		{0, 50, 0, 0.0713},
		{50, 50, 0.9287, 1},
		{9, 50, 0.0977, 0.3076},
		{5, 10, 0.2366, 0.7634},
	}
	for _, c := range cases {
		lo, hi := wilson(c.k, c.n)
		if math.Abs(lo-c.wantLo) > 0.001 || math.Abs(hi-c.want) > 0.001 {
			t.Errorf("wilson(%d,%d) = %.4f..%.4f, want %.4f..%.4f", c.k, c.n, lo, hi, c.wantLo, c.want)
		}
	}
	if lo, hi := wilson(0, 0); lo != 0 || hi != 1 {
		t.Errorf("wilson(0,0) = %v..%v, want the vacuous 0..1", lo, hi)
	}
}

func TestParseSizes(t *testing.T) {
	good := map[string][]int{"": nil, "1000": {1000}, "1000, 10000": {1000, 10000}, "5,,6": {5, 6}}
	for in, want := range good {
		got, err := parseSizes(in)
		if err != nil || len(got) != len(want) {
			t.Errorf("parseSizes(%q) = %v, %v; want %v", in, got, err, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseSizes(%q) = %v, want %v", in, got, want)
			}
		}
	}
	for _, bad := range []string{"abc", "0", "-5", "20001", "10,x"} {
		if _, err := parseSizes(bad); err == nil {
			t.Errorf("parseSizes(%q) should fail", bad)
		}
	}
}

func TestScratchDBGuard(t *testing.T) {
	ok := []string{
		"postgres://u:p@localhost:5432/rg_eval?sslmode=disable",
		"postgres://u:p@localhost:5432/replaygate_test",
		"postgres://u:p@localhost/EVAL",
	}
	for _, dsn := range ok {
		if err := scratchDB(dsn); err != nil {
			t.Errorf("%s should be allowed: %v", dsn, err)
		}
	}
	for _, dsn := range []string{
		"postgres://u:p@localhost:5432/replaygate",
		"postgres://u:p@localhost:5432/",
		"postgres://u:p@localhost:5432/prod",
	} {
		if err := scratchDB(dsn); err == nil {
			t.Errorf("%s must be refused", dsn)
		}
	}
	if err := scratchDB("://bad"); err == nil {
		t.Error("an unparsable URL must be refused")
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	env := func(string) string { return "" }
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no database", nil, "DATABASE_URL"},
		{"real database", []string{"-db", "postgres://u:p@h/replaygate"}, "scratch database"},
		{"bad sizes", []string{"-db", "postgres://u:p@h/rg_eval", "-bench", "x"}, "-bench"},
		{"zero runs", []string{"-db", "postgres://u:p@h/rg_eval", "-fp-runs", "0"}, "at least 1"},
		{"rare out of range", []string{"-db", "postgres://u:p@h/rg_eval", "-rare", "2"}, "between 0 and 1"},
		{"unreachable database", []string{"-db", "postgres://u:p@127.0.0.1:1/rg_eval?connect_timeout=1", "-bench", ""}, "eval failed"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := run(context.Background(), c.args, env, &out, &errb); code == 0 {
			t.Errorf("%s: exit 0, want failure", c.name)
		}
		if !strings.Contains(errb.String(), c.want) {
			t.Errorf("%s: stderr %q does not mention %q", c.name, errb.String(), c.want)
		}
	}
	var errb bytes.Buffer
	if code := run(context.Background(), []string{"-nonsense"}, env, &bytes.Buffer{}, &errb); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
}

func TestProblemsLists(t *testing.T) {
	var r Results
	if r.problems() != "" {
		t.Error("empty results have no problems")
	}
	r.Bugs = []DetectRow{{Runs: 3, Detected: 2, Located: 2}}
	r.Bugs[0].Spec.Name = "x"
	r.Benign = []BenignRow{{Runs: 3, Passed: 1}}
	r.Bench = []BenchRow{{Requests: 10, Verdict: "failed", Breaking: 4}}
	r.FP = []FPRow{{Label: "l", Errored: 1}}
	msg := r.problems()
	for _, want := range []string{"bug x caught 2/3", "control", "throughput run of 10 ended failed", "1 checks errored"} {
		if !strings.Contains(msg, want) {
			t.Errorf("problems() = %q, missing %q", msg, want)
		}
	}
}

// scratchSchemaURL gives the test its own schema in TEST_DATABASE_URL so it
// cannot collide with the other packages' tests running in parallel.
func scratchSchemaURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "ev_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "search_path=" + schema
}

// The whole evaluation, small: every seeded bug must be caught and named, the
// controls must pass, noise learning must beat no noise learning, and the
// reports must be written. This is also what keeps the ten-bug catalogue honest
// in CI: a change that stops the gate catching one fails here.
func TestEvaluationEndToEnd(t *testing.T) {
	dsn := scratchSchemaURL(t)
	dir := t.TempDir()
	mdPath, jsonPath := dir+"/evaluation.md", dir+"/results.json"

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{
		"-db", dsn, "-detect-runs", "1", "-fp-runs", "3", "-bench", "300",
		"-strict", "-out", mdPath, "-json", jsonPath,
	}, func(string) string { return "" }, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}

	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var res Results
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Bugs) != 10 || len(res.Benign) != 2 || len(res.FP) != 3 || len(res.Bench) != 1 {
		t.Fatalf("rows: %d bugs, %d controls, %d FP settings, %d bench", len(res.Bugs), len(res.Benign), len(res.FP), len(res.Bench))
	}
	for _, b := range res.Bugs {
		if b.Detected != 1 || b.Located != 1 {
			t.Errorf("%s: detected %d, named %d, want 1 and 1", b.Spec.Name, b.Detected, b.Located)
		}
	}
	if off, on := res.FP[0], res.FP[1]; off.Failed != off.Runs || on.Failed != 0 {
		t.Errorf("noise learning off failed %d/%d (want all), on failed %d/%d (want none)", off.Failed, off.Runs, on.Failed, on.Runs)
	}
	if b := res.Bench[0]; b.Verdict != "passed" || b.Replays != 900 || b.PerSecond <= 0 || b.Total <= 0 {
		t.Errorf("bench row %+v", b)
	}
	if res.Env.CPUs < 1 || !strings.HasPrefix(res.Env.Postgres, "PostgreSQL") {
		t.Errorf("environment not recorded: %+v", res.Env)
	}

	md, _ := os.ReadFile(mdPath)
	for _, want := range []string{"# Evaluation", "10 of 10 bugs caught", "## 2. False alarms", "## 3. Throughput", "## Limits of these numbers"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("report is missing %q", want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("with -out the report must not also go to stdout, got %d bytes", out.Len())
	}
}
