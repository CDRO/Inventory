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

	"github.com/CDRO/Inventory/internal/auth"
	"github.com/CDRO/Inventory/internal/config"
	"github.com/CDRO/Inventory/internal/consume"
	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/imagesearch"
	"github.com/CDRO/Inventory/internal/ingest"
	"github.com/CDRO/Inventory/internal/jobs"
	"github.com/CDRO/Inventory/internal/logging"
	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/migrate"
	"github.com/CDRO/Inventory/internal/notify"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/uploads"
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

// version is the build stamped in by the Dockerfile with
// -ldflags "-X main.version=…" (docs/specs/18-operations-and-observability.md).
//
// It stays "dev" for every `go build` and `go run` that does not pass the
// flag, which is every build outside the production image — so a binary
// reporting "dev" on GET /healthz is telling the truth about itself rather
// than reporting a version nobody stamped.
var version = "dev"

func main() {
	// Structured JSON to stdout, before anything can log
	// (docs/specs/18-operations-and-observability.md). Installed here rather
	// than inside serve() so the migration runner and the wizard write the
	// same shape into the same place; Docker's log driver does the rest.
	slog.SetDefault(logging.New(os.Stdout))

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, errorMessage(err))
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor maps a failure to the process exit code.
//
// Two failures get config.ExitConfig rather than a generic 1: missing
// configuration, and a database schema that does not match this binary
// (docs/specs/01-architecture-and-deployment.md,
// docs/specs/18-operations-and-observability.md). They share the code because
// they share the property that matters — both are fatal and non-retryable, and
// the operator must act before the container can ever succeed. Restart
// policies do not discriminate on exit code, so the distinct code is what lets
// an operator (and the logs) tell "the deployment is wrong, stop trying" apart
// from "crashed, worth restarting". Which of the two it was is in the message,
// which names the exact command that fixes it.
//
// **A database that is merely unreachable is not one of them.** That is an
// ordinary error and gets the generic 1, because it is genuinely worth
// retrying: PostgreSQL may still be starting.
func exitCodeFor(err error) int {
	var missing *config.MissingError
	if errors.As(err, &missing) {
		return config.ExitConfig
	}
	var schema *migrate.SchemaMismatchError
	if errors.As(err, &schema) {
		return config.ExitConfig
	}
	return 1
}

// errorMessage renders err for the operator. A configuration failure and a
// schema mismatch are printed bare, because each message is already the full
// remediation block and prefixing it would bury the first line.
func errorMessage(err error) string {
	var missing *config.MissingError
	if errors.As(err, &missing) {
		return missing.Error()
	}
	var schema *migrate.SchemaMismatchError
	if errors.As(err, &schema) {
		return schema.Error()
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
	case "recompute-progress":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runRecomputeProgress(ctx)
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
  serve                Run the HTTP server (default)
  setup                Interactive first-run wizard; writes .env
  migrate up           Apply pending database migrations
  migrate status       Show migration state
  recompute-progress   Rebuild gamification XP/level/streak from scratch
`)
}

// runRecomputeProgress rebuilds every user's cached gamification progress
// from inventory_logs and contribution_events (docs/specs/51-gamification-scoring.md).
// It is what makes "fully rebuildable" in that spec an operator-runnable
// fact rather than an aspiration — the same cache the nightly job in serve()
// refreshes automatically, exposed as a maintenance subcommand of the same
// binary (docs/specs/01-architecture-and-deployment.md) for a rule change or
// a data correction that cannot wait for 03:00.
func runRecomputeProgress(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	n, err := db.RecomputeAllProgress(ctx)
	if err != nil {
		return fmt.Errorf("recompute progress: %w", err)
	}
	fmt.Printf("inventory: recomputed progress for %d user(s)\n", n)
	return nil
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
	if err := migrate.Run(ctx, cfg.DatabaseURL, action, os.Stdout); err != nil {
		return err
	}
	if action != "up" {
		return nil
	}

	// Bootstrapping here as well as at serve is what makes the first deploy
	// work in one step. This is the command that brings the schema into
	// existence, so it is the one place the bootstrap is guaranteed to have
	// somewhere to write — and since serve now refuses to start against a
	// pending schema (docs/specs/18-operations-and-observability.md), it is
	// also the command that must run first on a fresh install.
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	return bootstrapAdmin(ctx, db, cfg)
}

// bootstrapAdmin creates the first admin on an install with no users at all
// (docs/specs/03-auth-and-multi-tenancy.md).
//
// It is idempotent — a no-op once any user exists — so running it from both
// `migrate up` and `serve` is safe, and whichever runs first on a fresh
// install does the work.
func bootstrapAdmin(ctx context.Context, db *store.Store, cfg *config.Config) error {
	created, err := auth.EnsureInitialAdmin(ctx, db, cfg.AdminInitialUsername, cfg.AdminInitialPassword)
	if err != nil {
		return fmt.Errorf("bootstrap initial admin: %w", err)
	}
	if created {
		fmt.Printf("inventory: created initial admin %q\n", cfg.AdminInitialUsername)
	}
	return nil
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

	// Before anything else touches the database: does its schema match this
	// binary (docs/specs/18-operations-and-observability.md)?
	//
	// Skipping `migrate up` during an upgrade used to produce a server that
	// started, then failed whichever query first met a missing column — a
	// 500 with a driver message, minutes or hours later, from a component
	// unrelated to the actual mistake. It is a fatal, named refusal instead,
	// with the exact command in the message, and a distinct exit code so the
	// restart policy's retries are visibly pointless rather than silently so.
	//
	// It runs before store.Open because it is the deploy order that is wrong,
	// not the connection: there is nothing to serve until it passes.
	if err := migrate.Check(ctx, cfg.DatabaseURL); err != nil {
		return err
	}

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

	// Non-fatal here, unlike in `migrate up`.
	//
	// Since the schema check above, a users table that does not exist is no
	// longer reachable — serve refuses to start before it gets here. What
	// remains is the ordinary run of database failures, and refusing to serve
	// the whole application because one bootstrap insert failed would be the
	// wrong trade: `migrate up` repeats the bootstrap, and an install that
	// already has users needs nothing from this call at all.
	if err := bootstrapAdmin(ctx, db, cfg); err != nil {
		slog.Warn("initial admin not created; `migrate up` will retry it", slog.Any("err", err))
	}

	// Background jobs (docs/specs/04-backend-api-conventions.md). Recover runs
	// before the listener and fails the pending jobs whose owning process is
	// gone — not every pending job, because a rolling update runs a second
	// instance next to the first on purpose (issue #121). Like the bootstrap it
	// is warned about rather than fatal: the schema is known to be current by
	// now, so a failure here is a database problem, and an unrecovered job is a
	// stuck review rather than a broken deployment.
	jobRunner := jobs.New(db, slog.Default())
	if err := jobRunner.Recover(ctx); err != nil {
		slog.Warn("could not recover interrupted jobs", slog.Any("err", err))
	}

	// Renews this process's claims on the jobs it is working, and fails the ones
	// whose owner stopped renewing. A goroutine like the sweeps below, not a
	// second job runner: it starts no work and calls no provider. It is what
	// recovers an instance that was killed while the other kept serving, which
	// no start-up will ever look at again.
	go jobRunner.RunLeases(ctx)

	// The matching service is shared with specs 06, 07 and 09; it is
	// constructed once here so all of them use the same thresholds.
	matcher := matching.New(db)

	// Photo ingestion (docs/specs/06-vision-shelf-ingestion.md). Typed as the
	// router's interfaces and assigned only on success: a nil *Service stored
	// in an interface would be non-nil, register the upload routes, and panic
	// on the first photo. An unusable upload volume disables uploads and
	// nothing else.
	var (
		ingester     httpapi.Ingester
		consumer     httpapi.Consumer
		photoStore   httpapi.PhotoStore
		productStore httpapi.PhotoStore
		ingestSweep  func(context.Context, time.Time) (int, error)
	)
	// Product pictures taken from a reviewed photo live apart from the photos
	// themselves: those are swept after review, these are kept for as long as
	// the product exists (docs/specs/07-shopping-list-reconciliation.md).
	if pictures, err := uploads.NewPictureDir(uploads.ProductImagesDir); err != nil {
		slog.Error("product pictures from photos disabled: upload volume unusable", slog.Any("err", err))
	} else {
		productStore = pictures
	}
	if photos, err := uploads.NewDir(uploads.IngestDir); err != nil {
		slog.Error("photo uploads disabled: upload volume unusable", slog.Any("err", err))
	} else {
		service := ingest.NewService(jobRunner, vision.NewClient(cfg.GeminiAPIKey), checker, matcher, db, photos, slog.Default())
		ingester, photoStore, ingestSweep = service, photos, service.SweepImages
		// Consumption photos share the same upload volume as shelf and
		// product photos (docs/specs/09-consumption-logging.md), so
		// ingestSweep above already covers their retention: its query is
		// kind-agnostic, matching every consumed job's image regardless of
		// which service created it.
		consumer = consume.NewService(jobRunner, vision.NewClient(cfg.GeminiAPIKey), checker, matcher, photos, slog.Default())
	}

	// Background removal (docs/specs/09-consumption-logging.md) exists only
	// when GEMINI_IMAGE_MODEL names a model. Unset is not a degraded state, so
	// it is not logged; an unusable upload volume is.
	var (
		backgrounds httpapi.BackgroundRemover
		cutoutStore httpapi.CutoutStore
	)
	if cfg.GeminiImageModel != "" {
		if dirs, err := uploads.NewJobDirs(uploads.CutoutsDir); err != nil {
			slog.Error("background removal disabled: upload volume unusable", slog.Any("err", err))
		} else {
			backgrounds = ingest.NewBackgrounds(vision.NewClient(cfg.GeminiAPIKey), checker, cfg.GeminiImageModel)
			cutoutStore = dirs
		}
	}

	go runStoreSweeps(ctx, db, ingestSweep)
	go runGamificationRecompute(ctx, db)
	go runWeeklyGamificationJobs(ctx, db)

	// Expiry notifications (docs/specs/17-expiry-notifications.md). The
	// service is constructed unconditionally: it has no provider, no key and
	// no volume to be missing, and with the table empty — which is the state
	// of every deployment that has not opted in — its hourly tick claims
	// nothing and posts nothing.
	notifier := notify.New(db, slog.Default())
	go runExpiryNotifications(ctx, notifier)

	srv := &http.Server{
		Addr: net.JoinHostPort("", cfg.HTTPPort),
		Handler: httpapi.NewRouter(httpapi.Deps{
			DB:          db,
			Vision:      checker,
			AdminVision: checker,
			Config:      cfg,
			// The one error serializer. cfg.IsDev() is the only thing that
			// decides whether an internal reason is ever disclosed.
			Errors:   httpapi.NewErrorWriter(cfg.IsDev(), slog.Default()),
			StaticFS: assets,
			// The same store backs the authorization gates and the handlers
			// behind them, so the two cannot be wired out of step.
			Store: db,
			// One matcher, shared by every flow that resolves text to products.
			Matcher:    matcher,
			Images:     suggester,
			ImageCache: imageCache,
			// Dev serves over plain http://localhost, where a Secure cookie
			// would never be sent back. Everywhere else Traefik terminates TLS.
			InsecureCookies: cfg.IsDev(),
			Ingester:        ingester,
			Photos:          photoStore,
			ProductImages:   productStore,
			Consumer:        consumer,
			Backgrounds:     backgrounds,
			Cutouts:         cutoutStore,
			Notifier:        notifier,
			// One completion line per request, and the build string on
			// /healthz and in the admin footer
			// (docs/specs/18-operations-and-observability.md).
			Logger:  slog.Default(),
			Version: version,
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		// A lifecycle event, so slog at info rather than fmt.Printf: the spec
		// puts startup and shutdown on the same structured stream as the
		// requests, so one `docker compose logs app` is the whole story
		// (docs/specs/18-operations-and-observability.md). The subcommands'
		// own fmt output stays as it is — that is a person's terminal, not a
		// log.
		slog.Info("listening",
			slog.String("port", cfg.HTTPPort),
			slog.String("env", cfg.AppEnv),
			slog.String("version", version))
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
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		// After the listener, so no new job can be submitted while the
		// in-flight ones are being told to stop. Anything that outlives the
		// grace period stays pending and the next start's Recover fails it.
		if err := jobRunner.Shutdown(shutdownCtx); err != nil {
			slog.Warn("jobs still running at shutdown", slog.Any("err", err))
		}
		return <-errCh
	}
}

// sweepInterval is how often expired rows are cleared. Nothing reads an expired
// row as valid — every lookup checks expiry itself — so this is housekeeping,
// and hourly is plenty.
const sweepInterval = time.Hour

// runStoreSweeps deletes rows past their retention: expired sessions and
// pairing codes, idempotency records older than their 7-day replay window
// (docs/specs/12-client-api-contract.md), old tombstones, and — when photo
// ingestion is enabled — the photos of jobs reviewed more than 30 days ago.
// Once at start, then every sweepInterval until ctx ends.
func runStoreSweeps(ctx context.Context, db *store.Store, ingestSweep func(context.Context, time.Time) (int, error)) {
	sweep := func() {
		now := time.Now()
		if ingestSweep != nil {
			if n, err := ingestSweep(ctx, now); err != nil {
				if ctx.Err() == nil {
					slog.Warn("sweep failed", slog.String("table", "job photos"), slog.Any("err", err))
				}
			} else if n > 0 {
				slog.Info("swept expired job photos", slog.Int("count", n))
			}
		}
		for name, run := range map[string]func() (int64, error){
			"sessions":            func() (int64, error) { return db.SweepSessions(ctx) },
			"pairing codes":       func() (int64, error) { return db.SweepPairingCodes(ctx) },
			"idempotency records": func() (int64, error) { return db.SweepIdempotencyRecords(ctx, now) },
			"tombstones":          func() (int64, error) { return db.SweepTombstones(ctx, now) },
		} {
			if n, err := run(); err != nil {
				if ctx.Err() == nil {
					slog.Warn("sweep failed", slog.String("table", name), slog.Any("err", err))
				}
			} else if n > 0 {
				slog.Info("swept expired rows", slog.String("table", name), slog.Int64("count", n))
			}
		}
	}

	sweep()
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// gamificationRecomputeHour is when the nightly pass runs, server-local
// (docs/specs/51-gamification-scoring.md): early enough that it is done well
// before anyone is awake to see a total settle slightly lower, late enough
// that the day it is rebuilding from is actually over.
const gamificationRecomputeHour = 3

// runGamificationRecompute rebuilds gamification progress once a day at
// docs/specs/51-gamification-scoring.md's fixed hour, consolidating the
// coalescing and dedup rules over the completed day. It runs once at start
// only if today's window has already passed and the process just started —
// otherwise it waits for the next occurrence, computed fresh each time
// rather than from a fixed-period ticker, so a DST transition cannot drift
// it away from the intended wall-clock hour.
func runGamificationRecompute(ctx context.Context, db *store.Store) {
	for {
		wait := time.Until(nextGamificationRecompute(time.Now()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		n, err := db.RecomputeAllProgress(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("nightly gamification recompute failed", slog.Any("err", err))
			}
			continue
		}
		slog.Info("recomputed gamification progress", slog.Int("users", n))

		// Storage-attributed achievements (docs/specs/52-gamification-quests-and-ui.md)
		// ride along on the same nightly cadence: health score and product
		// counts change slowly enough that once a day is honest, and it is one
		// recurring job rather than a second poll.
		if n, err := db.RunNightlyStorageAchievements(ctx); err != nil {
			if ctx.Err() == nil {
				slog.Warn("nightly storage achievement sweep failed", slog.Any("err", err))
			}
		} else {
			slog.Info("evaluated storage achievements", slog.Int("storages", n))
		}
	}
}

// runWeeklyGamificationJobs generates each storage's weekly quests, and
// evaluates the week that just ended for zero_waste_week, at Monday 00:00
// server-local (docs/specs/52-gamification-quests-and-ui.md). Computed fresh
// each time rather than from a fixed-period ticker, for the same
// DST-drift reason nextGamificationRecompute is.
func runWeeklyGamificationJobs(ctx context.Context, db *store.Store) {
	for {
		wait := time.Until(nextMonday(time.Now()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		n, err := db.RunWeeklyGamificationJobs(ctx, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("weekly quest generation failed", slog.Any("err", err))
			}
			continue
		}
		slog.Info("generated weekly quests", slog.Int("storages", n))
	}
}

// nextMonday returns the next occurrence of Monday 00:00 strictly after now,
// in now's own location.
func nextMonday(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for next.Weekday() != time.Monday || !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// nextGamificationRecompute returns the next occurrence of
// gamificationRecomputeHour:00 strictly after now, in now's own location.
func nextGamificationRecompute(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), gamificationRecomputeHour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
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

// runExpiryNotifications ticks once an hour and delivers whatever digests are
// due (docs/specs/17-expiry-notifications.md).
//
// The tick is hourly because send_hour is hourly; which storages are actually
// due is the store's decision, taken in one claiming statement, so this loop
// holds no state of its own and a restart cannot make it repeat a digest.
//
// The next tick is computed fresh from the wall clock each time rather than
// from a fixed-period ticker, for the same reason runGamificationRecompute
// does it: a ticker started at 07:59:30 fires at 08:59:30, 09:59:30 and so on,
// and a DST transition would drift it off the hour permanently.
//
// A tick's failure is logged and the loop carries on. There is no retry here
// by design — the spec is explicit that the next day's run is the retry, and
// growing an outbox for a convenience feature is the thing it forbids.
func runExpiryNotifications(ctx context.Context, notifier *notify.Service) {
	for {
		wait := time.Until(nextTopOfHour(time.Now()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		sent, err := notifier.RunDue(ctx, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("expiry notification run failed", slog.Any("err", err))
			}
			continue
		}
		if sent > 0 {
			slog.Info("sent expiry digests", slog.Int("storages", sent))
		}
	}
}

// nextTopOfHour returns the next :00 strictly after now, in now's own
// location.
func nextTopOfHour(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())
	for !next.After(now) {
		next = next.Add(time.Hour)
	}
	return next
}
