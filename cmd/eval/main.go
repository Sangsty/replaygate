// Command eval measures the gate: how many seeded bugs it catches, how often it
// raises a false alarm, and how fast it replays and diffs.
//
//	make eval
//
// It writes docs/evaluation.md and prints progress to stderr. It needs a
// scratch Postgres database (the name must contain "eval" or "test"), because it
// creates projects and thousands of recorded requests in it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/Sangsty/replaygate/internal/evalapp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", getenv("DATABASE_URL"), "scratch Postgres database (default: $DATABASE_URL)")
	detect := fs.Int("detect-runs", 3, "runs per seeded bug and per control")
	fp := fs.Int("fp-runs", 50, "clean-vs-clean runs per false-alarm setting")
	rare := fs.Float64("rare", 0.01, "probability that the intermittent noise field flips")
	bench := fs.String("bench", "1000,10000", "comma-separated request counts for the throughput runs (empty to skip)")
	conc := fs.Int("concurrency", 8, "requests in flight per replay worker")
	out := fs.String("out", "", "write the Markdown report to this file (default: stdout)")
	jsonOut := fs.String("json", "", "also write the raw results as JSON to this file")
	strict := fs.Bool("strict", false, "exit 1 if a bug is missed, a control fails, or a throughput run errors")
	anyDB := fs.Bool("any-db", false, "allow a database whose name does not contain eval or test")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *db == "" {
		fmt.Fprintln(stderr, "set DATABASE_URL (or -db) to a scratch database, e.g. the one scripts/eval.sh creates")
		return 2
	}
	if !*anyDB {
		if err := scratchDB(*db); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	sizes, err := parseSizes(*bench)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *detect < 1 || *fp < 1 || *rare < 0 || *rare > 1 || *conc < 1 {
		fmt.Fprintln(stderr, "-detect-runs, -fp-runs and -concurrency must be at least 1, and -rare between 0 and 1")
		return 2
	}

	cfg := Config{DetectRuns: *detect, FPRuns: *fp, Rare: *rare, BenchSizes: sizes, Bugs: evalapp.Bugs, Benign: evalapp.Benign}
	progress := func(s string) { fmt.Fprintln(stderr, "  "+s) }
	res, err := runEval(ctx, *db, cfg, *conc, progress)
	if err != nil {
		fmt.Fprintln(stderr, "eval failed:", err)
		return 1
	}

	w := stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer f.Close()
		w = f
	}
	res.Markdown(w)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if *strict {
		if msg := res.problems(); msg != "" {
			fmt.Fprintln(stderr, "strict: "+msg)
			return 1
		}
	}
	return 0
}

func runEval(ctx context.Context, dbURL string, cfg Config, conc int, progress func(string)) (Results, error) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	h, cleanup, err := newHarness(ctx, dbURL, conc, log)
	if err != nil {
		return Results{}, err
	}
	defer cleanup()

	res := Results{Env: h.environment(ctx), Config: cfg}
	progress("accuracy: seeded bugs, controls, false alarms")
	res.Bugs, res.Benign, res.FP, err = h.accuracy(ctx, cfg, progress)
	if err != nil {
		return Results{}, err
	}
	for _, n := range cfg.BenchSizes {
		progress(fmt.Sprintf("throughput: %d recorded requests", n))
		row, err := h.bench(ctx, n)
		if err != nil {
			return Results{}, err
		}
		progress(fmt.Sprintf("  %d replays in %s", row.Replays, secs(row.Total)))
		res.Bench = append(res.Bench, row)
	}
	return res, nil
}

// problems lists what a strict run objects to, or "" when everything is as
// expected. A false-alarm rate is reported, never enforced.
func (r Results) problems() string {
	var p []string
	for _, b := range r.Bugs {
		if b.Detected != b.Runs || b.Located != b.Runs {
			p = append(p, fmt.Sprintf("bug %s caught %d/%d, named %d/%d", b.Spec.Name, b.Detected, b.Runs, b.Located, b.Runs))
		}
	}
	for _, b := range r.Benign {
		if b.Passed != b.Runs {
			p = append(p, fmt.Sprintf("control %s passed %d/%d", b.Spec.Name, b.Passed, b.Runs))
		}
	}
	for _, b := range r.Bench {
		if b.Verdict != "passed" {
			p = append(p, fmt.Sprintf("throughput run of %d ended %s (breaking=%d)", b.Requests, b.Verdict, b.Breaking))
		}
	}
	for _, f := range r.FP {
		if f.Errored > 0 {
			p = append(p, fmt.Sprintf("%d checks errored in %q", f.Errored, f.Label))
		}
	}
	return strings.Join(p, "; ")
}

// scratchDB refuses databases that do not look disposable.
func scratchDB(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("DATABASE_URL is not a valid URL: %w", err)
	}
	name := strings.ToLower(strings.TrimPrefix(u.Path, "/"))
	if strings.Contains(name, "eval") || strings.Contains(name, "test") {
		return nil
	}
	return errors.New("refusing to run: the eval creates projects and thousands of requests, so use a scratch database whose name contains \"eval\" or \"test\" (scripts/eval.sh makes one), or pass -any-db")
}

func parseSizes(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > 20000 {
			return nil, fmt.Errorf("-bench: %q is not a request count between 1 and 20000 (the API's sample limit)", f)
		}
		out = append(out, n)
	}
	return out, nil
}
