package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pressly/goose/v3"
)

// classicMarker is the exact line decision D3
// (docs/plans/2026-09-harness-optimization.md, docs/specs/38-release-pipeline-and-nas-runner.md)
// fixes as the classic-deploy signal.
const classicMarker = "-- +inventory:classic"

// classicNamespace is the directive prefix this package owns. It sits outside
// goose's own "-- +goose" namespace on purpose (D3), so the pinned goose never
// mistakes it for a directive — but that also means goose does not validate it
// for us, so ParseClassicMarker treats any line that starts with this prefix
// and is not the exact, correctly placed classicMarker as a mistake to report
// rather than a comment to ignore.
const classicNamespace = "-- +inventory:"

// classicNamespacePattern detects an *attempt* at classicNamespace even when
// the whitespace between "--" and "+inventory:" itself is irregular — doubled
// ("--  +inventory:classic") or absent ("--+inventory:classic") — one level
// more precise than the outer TrimSpace/ToLower normalization below, which
// only covers whitespace and case around the whole line, not inside the
// prefix. \s* rather than \s+ deliberately also matches zero whitespace.
var classicNamespacePattern = regexp.MustCompile(`^--\s*\+inventory:`)

// gooseUpMarker is the directive goose itself requires to start an Up block.
const gooseUpMarker = "-- +goose Up"

// PlanClassicError is returned by Plan when at least one pending migration
// carries the classic marker.
//
// Classic is not a failure — it is one of Plan's two ordinary outcomes,
// alongside rolling — but the consumer, deploy/synology/update, is a `sh`
// script under `set -eu` that reads only the process's exit code, because
// parsing stdout would mean parsing past `docker-compose run`'s own output
// (docs/specs/38-release-pipeline-and-nas-runner.md). Returning an error is
// what lets Plan's caller in cmd/inventory turn that outcome into exit
// ExitClassic without also treating rolling as an error. Plan has already
// written the full, human-readable report to its out before returning this,
// so cmd/inventory prints nothing further for it.
type PlanClassicError struct{}

// Error satisfies the error interface. cmd/inventory does not print it: the
// report Plan already wrote to stdout is the whole story.
func (*PlanClassicError) Error() string { return "migrate plan: classic deploy required" }

// ExitClassic is the process exit code for PlanClassicError
// (docs/specs/38-release-pipeline-and-nas-runner.md, decision D3). Chosen to
// stay clear of 1 (generic error), 2 (shell misuse) and 78
// (config.ExitConfig, already claimed by configuration and schema-mismatch
// failures).
const ExitClassic = 3

// MarkerPlacementError is a line in the classic-marker namespace
// ("-- +inventory:…") found somewhere other than its one required position —
// the first non-blank line after "-- +goose Up" — or found there but not
// matching the marker exactly. Both are reported rather than read as "no
// marker": a marker a person meant to write and got wrong, or put in the
// wrong place, must never be silently treated as a migration that carries
// none (D3).
type MarkerPlacementError struct {
	// Line is the 1-indexed line number the offending text was found on.
	Line int
}

// Error names the line, since Plan's caller has nothing else to go on.
func (e *MarkerPlacementError) Error() string {
	return fmt.Sprintf(
		"classic marker on line %d is not the exact line %q as the first non-blank line after %q",
		e.Line, classicMarker, gooseUpMarker)
}

// ParseClassicMarker reports whether a migration file's content requires a
// classic deploy: the pending-migration half of decision D3
// (docs/specs/38-release-pipeline-and-nas-runner.md). The marker is the exact
// line
//
//	-- +inventory:classic
//
// as the first non-blank line after "-- +goose Up".
//
// "Exact" is about the characters of the line, not its terminator: each
// line's trailing "\r" is stripped before comparison, so a CRLF checkout is
// tolerated — nothing pins migrations/** to LF, and this repository is
// developed on Windows with core.autocrlf=true. Nothing else is tolerated: no
// leading whitespace, no trailing space or tab, no trailing comment, no
// second statement on the line.
//
// A line that starts with the "-- +inventory:" namespace this package owns
// but is not the exact, correctly placed marker — wrong position, leading or
// trailing whitespace, wrong case, trailing text, a typo in the directive
// name — is reported as a
// *MarkerPlacementError rather than silently treated as no marker at all.
func ParseClassicMarker(content []byte) (classic bool, err error) {
	rawLines := strings.Split(string(content), "\n")
	lines := make([]string, len(rawLines))
	for i, line := range rawLines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}

	upLine := -1
	for i, line := range lines {
		if line == gooseUpMarker {
			upLine = i
			break
		}
	}

	requiredLine := -1
	if upLine >= 0 {
		for i := upLine + 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "" {
				continue
			}
			requiredLine = i
			break
		}
	}

	for i, line := range lines {
		// Detecting an *attempt* at the marker has to be looser than
		// accepting one: TrimSpace and ToLower catch a line indented under
		// "-- +goose Up" (this repository's own SQL style) or typed in the
		// wrong case, neither of which goose itself would reject either,
		// since it is not goose's own directive. Without the trim, a line
		// like "  -- +inventory:classic" starts with neither the exact
		// marker nor classicNamespace, so it fell through both switch cases
		// silently — read as no marker at all rather than reported — which
		// is exactly the silent failure decision D3 exists to prevent.
		// Acceptance itself stays byte-exact: only line == classicMarker,
		// untrimmed, ever sets classic true.
		namespaced := classicNamespacePattern.MatchString(strings.ToLower(strings.TrimSpace(line)))
		switch {
		case i == requiredLine && line == classicMarker:
			classic = true
		case namespaced:
			return false, &MarkerPlacementError{Line: i + 1}
		}
	}
	return classic, nil
}

// migrationFile is one shipped migration: its filename (for reporting) and
// the numeric version goose.NumericComponent parses from it (for comparing
// against what the database has applied).
type migrationFile struct {
	name    string
	version int64
}

// shippedMigrationFiles lists the migration files in dir, ascending by
// version. A file goose would reject is skipped, for the same reason
// shippedVersions (check.go) skips it: a malformed filename is `migrate up`'s
// failure to report, with goose's own message, not a distortion of this
// comparison's counts.
func shippedMigrationFiles(dir string) ([]migrationFile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, fmt.Errorf("migrate: scan %s: %w", dir, err)
	}

	files := make([]migrationFile, 0, len(matches))
	for _, match := range matches {
		name := filepath.Base(match)
		version, err := goose.NumericComponent(strings.TrimSpace(name))
		if err != nil {
			continue
		}
		files = append(files, migrationFile{name: name, version: version})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}

// Plan lists the pending migrations and reports whether applying them needs a
// rolling or a classic deploy (docs/specs/38-release-pipeline-and-nas-runner.md).
// It writes its report to out in the exact shape the spec fixes:
//
//	migrate plan: rolling|classic|nothing pending (<n> pending: 00015_x.sql, …)
//
// followed by one line per pending file, or exactly
//
//	migrate plan: nothing pending
//
// with no file lines when there is nothing pending.
//
// Only pending migrations are read for the marker — an already-applied
// migration's marker, well-formed or not, changes nothing, because the
// question Plan answers only ever concerns migrations that have not run yet.
//
// Plan returns:
//   - nil for rolling and for nothing pending;
//   - *PlanClassicError when at least one pending migration carries the
//     marker, after writing the report;
//   - *MarkerPlacementError when the marker namespace appears anywhere but
//     its one required position, before writing any report;
//   - *SchemaMismatchError (the same type and message `serve` refuses to
//     start against, docs/specs/18-operations-and-observability.md) when the
//     database has applied a migration this binary does not ship — cmd/inventory
//     maps that to config.ExitConfig, the same as every other fatal,
//     non-retryable deployment mismatch;
//   - any other error unwrapped — a database that cannot be reached, or a
//     migrations directory this binary was not packaged with — for the
//     generic exit code, because those are worth retrying rather than acting
//     on.
func Plan(ctx context.Context, dsn string, out io.Writer) error {
	dir, err := resolveDir()
	if err != nil {
		return err
	}

	files, err := shippedMigrationFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintln(out, "migrate plan: nothing pending")
		return nil
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrate: open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	applied, highest, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	binaryVersion := files[len(files)-1].version

	var pending []migrationFile
	for _, f := range files {
		if !applied[f.version] {
			pending = append(pending, f)
		}
	}

	// Ahead of the binary is spec 18's fatal, named error — the same one
	// `serve` refuses to start against — not something Plan can have an
	// opinion about deploying. It is checked before anything about pending
	// files, because there is nothing to plan when the database already
	// holds a migration this binary has never heard of.
	if highest > binaryVersion {
		return &SchemaMismatchError{DBVersion: highest, BinaryVersion: binaryVersion, Pending: len(pending)}
	}

	if len(pending) == 0 {
		fmt.Fprintln(out, "migrate plan: nothing pending")
		return nil
	}

	classic := false
	for _, f := range pending {
		content, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", f.name, err)
		}
		marked, err := ParseClassicMarker(content)
		if err != nil {
			return fmt.Errorf("migrate plan: %s: %w", f.name, err)
		}
		if marked {
			classic = true
		}
	}

	names := make([]string, len(pending))
	for i, f := range pending {
		names[i] = f.name
	}

	mode := "rolling"
	if classic {
		mode = "classic"
	}
	fmt.Fprintf(out, "migrate plan: %s (%d pending: %s)\n", mode, len(pending), strings.Join(names, ", "))
	for _, f := range pending {
		fmt.Fprintf(out, "  %s\n", f.name)
	}

	if classic {
		return &PlanClassicError{}
	}
	return nil
}
