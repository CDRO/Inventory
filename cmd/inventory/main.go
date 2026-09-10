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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CDRO/Inventory/internal/config"
	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/imagesearch"
	"github.com/CDRO/Inventory/internal/matching"
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
		fmt.Fprintln(os.Stderr, errorMessage(err))
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor maps a failure to the process exit code.
//
// A configuration failure gets config.ExitConfig rather than a generic 1
// because it is fatal and non-retryable: the operator must act before the
// container can ever succeed. Restart policies do not discriminate on exit
// code, so the distinct code is what lets an operator (and the logs) tell
// "misconfigured, stop trying" apart from "crashed, worth restarting"
// (docs/specs/01-architecture-and-deployment.md).
func exitCodeFor(err error) int {
	var missing *config.MissingError
	if errors.As(err, &missing) {
		return config.ExitConfig
	}
	return 1
}

// errorMessage renders err for the operator. A configuration failure is
// printed bare, because its message is already the full remediation block and
// prefixing it would bury the first line.
func errorMessage(err error) string {
	var missing *config.MissingError
	if errors.As(err, &missing) {
		return missing.Error()
	}
	return "inventory: " + err.Error()
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
		// Signal-aware: a long `migrate up` should respond to docker stop.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runMigrate(ctx, args[1:])
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

func runMigrate(ctx context.Context, args []string) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	action := "up"
	if len(args) > 0 {
		action = args[0]
	}
	return migrate.Run(ctx, cfg.DatabaseURL, action, os.Stdout)
}

// suggestionCacheDir is where downloaded image suggestions live
// (docs/specs/07-shopping-list-reconciliation.md).
//
// It is a constant rather than a configuration knob because it is a container
// path, not a deployment choice: docker-compose.yml mounts a volume there, and
// making it settable would invite a value the compose file does not mount, at
// which point the cache silently lives in the container's writable layer and
// disappears on the next deploy.
const suggestionCacheDir = "/data/cache/imagesearch"

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

	// The suggestion-image cache and the providers behind it
	// (docs/specs/07-shopping-list-reconciliation.md). SerpAPI is optional:
	// without a key the New Item flow degrades to icon-only suggestions rather
	// than failing, which is the documented behaviour for an unavailable
	// provider.
	imageCache := imagesearch.NewCache(suggestionCacheDir, db, nil, slog.Default())
	suggester := imagesearch.NewService(
		imagesearch.NewIconify(nil),
		imagesearch.NewSerpAPI(cfg.SerpAPIKey, nil),
		imageCache,
		slog.Default(),
	)

	// Cap enforcement and orphan collection. Started before the listener so a
	// crash's leftovers are cleaned at boot rather than up to an hour later.
	go imageCache.RunSweeps(ctx)

	srv := &http.Server{
		Addr: net.JoinHostPort("", cfg.HTTPPort),
		Handler: httpapi.NewRouter(httpapi.Deps{
			DB:     db,
			Vision: checker,
			// The one error serializer. cfg.IsDev() is the only thing that
			// decides whether an internal reason is ever disclosed.
			Errors:   httpapi.NewErrorWriter(cfg.IsDev(), slog.Default()),
			StaticFS: assets,
			// The same store backs the authorization gates and the handlers
			// behind them, so the two cannot be wired out of step.
			Store: db,
			// The matching service is shared with specs 06 and 09; it is
			// constructed once here so all of them use the same thresholds.
			Matcher:    matching.New(db),
			Images:     suggester,
			ImageCache: imageCache,
		}),
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
