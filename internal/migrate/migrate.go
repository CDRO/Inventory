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
func Run(ctx context.Context, dsn, action string, out io.Writer) error {
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
	// goose reports through its logger, not its return value: `status` and
	// `version` produce their entire output that way. A NopLogger here — which
	// is what this was until the first migrations existed to report on — makes
	// those two subcommands print nothing at all and look broken.
	goose.SetLogger(writerLogger{out: out})

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

// writerLogger adapts an io.Writer to goose's logger interface.
//
// Fatalf deliberately does not exit the process: goose calls it for migration
// failures, and killing the process there would skip the deferred database
// close and lose the error's place in the caller's own reporting. The
// corresponding Run call returns an error anyway.
type writerLogger struct {
	out io.Writer
}

func (l writerLogger) Printf(format string, v ...any) {
	fmt.Fprintf(l.out, format, v...)
}

func (l writerLogger) Fatalf(format string, v ...any) {
	fmt.Fprintf(l.out, format, v...)
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
