// Package migrate applies the goose SQL migrations under /migrations.
//
// It is a subcommand of the application binary rather than a separate tool so
// that migrating needs no host toolchain — the same image that serves traffic
// applies the schema (docs/specs/01-architecture-and-deployment.md).
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
)

// Candidate migration directories, in order. The first is where the production
// image keeps them; the second is the repository layout used in dev, where the
// source tree is bind-mounted at /src.
//
// There is deliberately no environment override. The two locations cover both
// images the project ships, and an undocumented knob that silently repoints the
// schema is worse than no knob at all.
var candidateDirs = []string{"/migrations", "migrations"}

// Run applies action ("up", "down", "status", "version") against dsn.
func Run(ctx context.Context, dsn, action string, out io.Writer) (err error) {
	// goose reports "status" and "version" through its Logger, not through a
	// return value, so a Logger that discards output (goose.NopLogger, the
	// previous setting here) makes both subcommands silent. fatalLogger fixes
	// that by writing Printf output to out, and additionally turns Fatalf into
	// a panic this function recovers into a real error: goose's own default
	// logger calls os.Exit(1) from Fatalf, and every internal caller of it
	// assumes the process stops right there. A Logger that only prints from
	// Fatalf — the obvious naive fix — would let a Fatalf-worthy failure
	// return nil, meaning a failed `migrate up` could report success. No
	// caller in the pinned goose version reaches Fatalf today (Up, Status and
	// Version all fail through returned errors instead), but the interface
	// exists and a future goose upgrade must not silently regress this
	// guarantee.
	defer recoverFatal(&err)

	dir, err := resolveDir()
	if err != nil {
		return err
	}

	count, err := countMigrations(dir)
	if err != nil {
		return err
	}
	if count == 0 {
		// The schema itself arrives with docs/specs/02-data-model.md. Saying so
		// is better than goose's bare "no migration files found", which reads
		// like a packaging bug.
		fmt.Fprintf(out, "No migrations in %s yet; nothing to apply.\n", dir)
		return nil
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrate: open database: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			fmt.Fprintf(out, "migrate: closing database: %v\n", cerr)
		}
	}()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("migrate: set dialect: %w", err)
	}
	// Printf forwarding is scoped to status/version: those two have no other
	// way to report anything (see fatalLogger's doc comment), but up/down
	// already print their own one-line summary below, and goose.UpContext
	// separately Printf's a line per applied migration plus a
	// "successfully migrated" line — forwarding those too would make a happy
	// path noisy for no reason. Fatalf's panic/recover safety net stays on
	// for every action regardless.
	goose.SetLogger(&fatalLogger{out: out, verbose: action == "status" || action == "version"})

	switch action {
	case "up":
		if err := goose.UpContext(ctx, db, dir); err != nil {
			return fmt.Errorf("migrate: up: %w", err)
		}
		fmt.Fprintf(out, "Applied migrations from %s.\n", dir)
	case "down":
		if err := goose.DownContext(ctx, db, dir); err != nil {
			return fmt.Errorf("migrate: down: %w", err)
		}
		fmt.Fprintln(out, "Rolled back one migration.")
	case "status":
		if err := goose.StatusContext(ctx, db, dir); err != nil {
			return fmt.Errorf("migrate: status: %w", err)
		}
	case "version":
		if err := goose.VersionContext(ctx, db, dir); err != nil {
			return fmt.Errorf("migrate: version: %w", err)
		}
	default:
		return fmt.Errorf("migrate: unknown action %q (want up, down, status or version)", action)
	}
	return nil
}

// fatalLog carries a goose Logger.Fatalf message across the panic/recover
// boundary in Run.
type fatalLog string

// fatalLogger implements goose.Logger. When verbose, Printf writes go to
// out — this is what makes `migrate status` and `migrate version` produce
// output at all, since goose renders both through Printf rather than a
// return value. When not verbose, Printf is silently discarded: up/down
// have their own one-line summaries and goose's internal per-migration
// Printf calls would otherwise make their happy path noisy. Fatalf always
// panics, verbose or not, with a fatalLog that Run recovers into a returned
// error instead of letting goose's stdlib default (os.Exit(1)) end the
// process directly.
type fatalLogger struct {
	out     io.Writer
	verbose bool
}

func (l *fatalLogger) Printf(format string, v ...any) {
	if !l.verbose {
		return
	}
	fmt.Fprintf(l.out, format, v...)
}

func (l *fatalLogger) Fatalf(format string, v ...any) {
	panic(fatalLog(fmt.Sprintf(format, v...)))
}

// recoverFatal converts a panic carrying a fatalLog (from fatalLogger.Fatalf)
// into *err. Any other panic value is re-raised unchanged — this only ever
// intercepts the one panic shape this package itself produces.
func recoverFatal(err *error) {
	if r := recover(); r != nil {
		msg, ok := r.(fatalLog)
		if !ok {
			panic(r)
		}
		*err = fmt.Errorf("migrate: %s", string(msg))
	}
}

// resolveDir picks the first candidate directory that exists.
func resolveDir() (string, error) {
	for _, dir := range candidateDirs {
		if isDir(dir) {
			return dir, nil
		}
	}
	return "", errors.New("migrate: no migrations directory found (looked in /migrations and ./migrations)")
}

func countMigrations(dir string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return 0, fmt.Errorf("migrate: scan %s: %w", dir, err)
	}
	return len(matches), nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
