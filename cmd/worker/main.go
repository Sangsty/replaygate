// Command worker is the runner-side replay worker. It claims replay jobs for
// one variant from the replaygate API, sends the recorded requests to the
// target build, and uploads what came back.
//
// Run one worker per build under test: baseline_a and baseline_b against the
// baseline build, candidate against the PR build.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Sangsty/replaygate/internal/config"
	"github.com/Sangsty/replaygate/internal/recorder"
	"github.com/Sangsty/replaygate/internal/replayer"
	"github.com/Sangsty/replaygate/internal/sanitize"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if err := config.LoadDotEnv(".env"); err != nil {
		return err
	}
	var env config.Env
	apiURL := env.Require("RG_API_URL")
	projectID := env.Require("RG_PROJECT_ID")
	apiKey := env.Require("RG_API_KEY")
	variant := env.Require("RG_VARIANT")
	targetRaw := env.Require("RG_TARGET_URL")
	if err := env.Err(); err != nil {
		return err
	}
	switch variant {
	case "baseline_a", "baseline_b", "candidate":
	default:
		return fmt.Errorf("RG_VARIANT must be baseline_a, baseline_b or candidate, got %q", variant)
	}
	target, err := url.Parse(targetRaw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return fmt.Errorf("RG_TARGET_URL must be an absolute http(s) URL, got %q", targetRaw)
	}

	// Optional settings. RG_REPLAY_AUTH is the Authorization header value the
	// target expects, for example "Bearer <token>". It lives only in this
	// process's environment: it is never uploaded or stored.
	auth := os.Getenv("RG_REPLAY_AUTH")
	hostname, _ := os.Hostname()
	workerID := env.Default("RG_WORKER_ID", fmt.Sprintf("%s-%s-%d", hostname, variant, os.Getpid()))
	concurrency, err := intEnv("RG_CONCURRENCY", 8)
	if err != nil {
		return err
	}
	idleExit, err := durationEnv("RG_IDLE_EXIT", 0)
	if err != nil {
		return err
	}
	timeout, err := durationEnv("RG_REQUEST_TIMEOUT", 10*time.Second)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Response bodies are redacted here, on the runner, with the project's
	// rules, before anything is uploaded.
	rulesCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	rules, err := recorder.NewClient(apiURL, projectID, apiKey).Rules(rulesCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("fetch sanitizer rules: %w", err)
	}
	san, err := sanitize.New(rules)
	if err != nil {
		return fmt.Errorf("project sanitizer rules are invalid: %w", err)
	}

	w := &replayer.Worker{
		Client: replayer.NewClient(apiURL, projectID, apiKey),
		Replayer: &replayer.Replayer{
			Target: target, AuthHeader: auth, Sanitizer: san, Concurrency: concurrency, Timeout: timeout,
		},
		Log: log, ID: workerID, Variant: variant, IdleExit: idleExit,
	}
	log.Info("worker started", "id", workerID, "variant", variant, "target", target.Redacted(), "auth_configured", auth != "")

	stats, err := w.Run(ctx)
	log.Info("worker stopped", "jobs", stats.Jobs, "requests", stats.Requests, "request_errors", stats.Failures)
	if errors.Is(err, replayer.ErrPermanent) {
		return fmt.Errorf("the API rejected this worker (check RG_API_KEY and RG_PROJECT_ID): %w", err)
	}
	return err
}

func intEnv(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, v)
	}
	return n, nil
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a duration such as 30s, got %q", name, v)
	}
	return d, nil
}
