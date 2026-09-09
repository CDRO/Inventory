// Command inventory is the single binary behind every part of the system: the
// HTTP server, the interactive first-run wizard, and the migration runner.
//
// Shipping one artifact is what makes the deployment story hold — the same
// binary that serves traffic also writes .env and applies migrations, so there
// is no second toolchain to install on the host
// (docs/specs/01-architecture-and-deployment.md).
//
// Usage:
//
//	inventory serve      # default; run the HTTP server
//	inventory setup      # interactive .env wizard
//	inventory migrate up # apply database migrations
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CDRO/Inventory/internal/config"
	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/migrate"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
	"github.com/CDRO/Inventory/web"
)

// Server timeouts. ReadHeader and Idle are deliberately short; WriteTimeout is
// generous because shelf-photo uploads are large
// (docs/specs/04-backend-api-conventions.md).
const (
	readHeaderTimeout = 10 * time.Second
	writeTimeout      = 120 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownGrace     = 15 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// A configuration failure is fatal and non-retryable: it exits with a
		// distinct code so `restart: unless-stopped` does not spin the
		// container and scroll the remediation out of `docker compose logs`.
		var missing *config.MissingError
		if errors.As(err, &missing) {
			fmt.Fprintln(os.Stderr, missing.Error())
			os.Exit(config.ExitConfig)
		}
		fmt.Fprintf(os.Stderr, "inventory: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}

	switch command {
	case "serve":
		return serve()
	case "setup":
		return (&config.Wizard{}).Run()
	case "migrate":
		return runMigrate(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `inventory — household inventory system

Commands:
  serve            Run the HTTP server (default)
  setup            Interactive first-run wizard; writes .env
  migrate up       Apply pending database migrations
  migrate status   Show migration state
`)
}

func runMigrate(args []string) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	action := "up"
	if len(args) > 0 {
		action = args[0]
	}
	return migrate.Run(context.Background(), cfg.DatabaseURL, action, os.Stdout)
}

func serve() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	// The root context is cancelled on SIGINT/SIGTERM so an in-flight shutdown
	// stops the background work as well as the listener.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	assets, err := staticFS(cfg)
	if err != nil {
		return err
	}

	checker := vision.NewChecker(db, vision.NewAPILister(cfg.GeminiAPIKey), cfg.GeminiModel)

	srv := &http.Server{
		Addr:              net.JoinHostPort("", cfg.HTTPPort),
		Handler:           httpapi.NewRouter(httpapi.Deps{DB: db, Vision: checker, StaticFS: assets}),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("inventory: listening on :%s (env=%s)\n", cfg.HTTPPort, cfg.AppEnv)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Println("inventory: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		return <-errCh
	}
}

// staticFS resolves the asset tree: STATIC_DIR reads from disk so a frontend
// edit needs only a browser refresh, and an empty value serves the copy
// embedded in the binary.
func staticFS(cfg *config.Config) (fs.FS, error) {
	if cfg.StaticDir != "" {
		return os.DirFS(cfg.StaticDir), nil
	}
	assets, err := web.Static()
	if err != nil {
		return nil, fmt.Errorf("load embedded assets: %w", err)
	}
	return assets, nil
}
