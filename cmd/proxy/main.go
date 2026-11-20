// Command proxy runs the recorder: a reverse proxy that forwards traffic to an
// upstream service and records sanitized GET requests through the replaygate API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sangsty/replaygate/internal/config"
	"github.com/Sangsty/replaygate/internal/recorder"
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
	upstreamRaw := env.Require("RG_UPSTREAM_URL")
	apiURL := env.Require("RG_API_URL")
	projectID := env.Require("RG_PROJECT_ID")
	apiKey := env.Require("RG_API_KEY")
	addr := env.Default("RG_PROXY_ADDR", ":8081")
	if err := env.Err(); err != nil {
		return err
	}

	upstream, err := url.Parse(upstreamRaw)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" {
		return fmt.Errorf("RG_UPSTREAM_URL must be an absolute http(s) URL, got %q", upstreamRaw)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := recorder.NewClient(apiURL, projectID, apiKey)
	batcher := recorder.NewBatcher(client, recorder.BatcherConfig{}, log)
	proxy := recorder.NewProxy(recorder.Options{Upstream: upstream, Sink: batcher, Logger: log})

	// Traffic is forwarded immediately; recording starts once the project's
	// sanitizer rules have been fetched, and rules are refreshed every minute.
	go proxy.RefreshRules(ctx, client.Rules, time.Minute, 2*time.Second)

	// No Read/WriteTimeout: they would cut off slow clients and large or
	// streaming upstream responses. ReadHeaderTimeout still guards the handshake.
	srv := &http.Server{
		Addr:              addr,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("recorder listening", "addr", addr, "upstream", upstream.String())
		errCh <- srv.ListenAndServe()
	}()

	var serveErr error
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		serveErr = srv.Shutdown(shutdownCtx)
	}

	// Flush whatever is still queued before exiting.
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := batcher.Close(flushCtx); err != nil {
		log.Warn("flush on shutdown timed out", "err", err)
	}
	log.Info("recorder stopped", "uploaded", batcher.Uploaded(), "dropped", batcher.Dropped())
	return serveErr
}
