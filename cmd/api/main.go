// Command api runs the replaygate REST API and its admin commands.
//
// Usage:
//
//	api serve                   run migrations, then serve the API (default)
//	api bootstrap -name demo    create a dev project and print its API key once
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sangsty/replaygate/internal/config"
	"github.com/Sangsty/replaygate/internal/server"
	"github.com/Sangsty/replaygate/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(os.Args[1:], log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(args []string, log *slog.Logger) error {
	if err := config.LoadDotEnv(".env"); err != nil {
		return err
	}
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(log)
	case "bootstrap":
		return bootstrap(args)
	default:
		return fmt.Errorf("unknown command %q (want serve or bootstrap)", cmd)
	}
}

func serve(log *slog.Logger) error {
	var env config.Env
	dbURL := env.Require("DATABASE_URL")
	addr := env.Default("RG_API_ADDR", ":8080")
	if err := env.Err(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := store.Migrate(ctx, st.Pool()); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(st, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// bootstrap creates a project owned by a local development user and prints a
// fresh API key. It exists so the recorder can be used before GitHub login is
// built (week 3); real users will own their projects through OAuth.
func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	name := fs.String("name", "demo", "project name")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var env config.Env
	dbURL := env.Require("DATABASE_URL")
	if err := env.Err(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := store.Migrate(ctx, st.Pool()); err != nil {
		return err
	}

	// GitHub user ids start at 1, so id 0 can never collide with a real user.
	owner, err := st.UpsertUser(ctx, 0, "bootstrap")
	if err != nil {
		return err
	}
	project, err := st.CreateProject(ctx, owner, *name)
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("project %q already exists; pick another with -name", *name)
	}
	if err != nil {
		return err
	}
	key, err := st.CreateAPIKey(ctx, project.ID, "bootstrap")
	if err != nil {
		return err
	}

	// stdout, not the logger: this is the one time the key is shown.
	fmt.Printf("Project %q created.\n\n", project.Name)
	fmt.Printf("RG_PROJECT_ID=%s\n", project.ID)
	fmt.Printf("RG_API_KEY=%s\n\n", key)
	fmt.Println("Save the key now. Only its hash is stored, so it cannot be shown again.")
	return nil
}
