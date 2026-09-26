package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
)

// undefinedTable is PostgreSQL's SQLSTATE for "relation does not exist" — how
// a database that has never been migrated answers a read of goose's own
// bookkeeping table.
const undefinedTable = "42P01"

// SchemaMismatchError is the database's schema and the binary's migrations
// disagreeing (docs/specs/18-operations-and-observability.md).
//
// Two shapes, one type: the schema is *behind* the binary because somebody
// skipped `migrate up` during an upgrade, or *ahead* of it because a newer
// dump was restored or an older image redeployed. Both are fatal at startup,
// and neither is a condition that retrying fixes — which is the whole reason
// they are named rather than left to surface as "column does not exist" from
// whichever query happened to run first.
type SchemaMismatchError struct {
	// DBVersion is the highest migration the database has applied, 0 for a
	// database that has never been migrated.
	DBVersion int64
	// BinaryVersion is the highest migration this binary ships.
	BinaryVersion int64
	// Pending counts the shipped migrations the database has not applied. It
	// is zero when the database is ahead.
	Pending int
}

// Behind reports whether the database is missing migrations this binary ships.
func (e *SchemaMismatchError) Behind() bool { return e.Pending > 0 }

// Error is the operator-facing remediation block, printed bare by
// cmd/inventory exactly as the missing-configuration message is: it is already
// the whole instruction, and prefixing it would bury the first line.
func (e *SchemaMismatchError) Error() string {
	if e.Behind() {
		// Neither line names a compose invocation, and each is wrong on the
		// operator's own Synology NAS variant for a different reason: "docker
		// compose -f docker-compose.yml run --rm app migrate up" (missing the
		// NAS's second -f layer) resolves `db` to a throwaway named volume
		// instead of the NAS's bind-mounted ./pgdata, and reports success
		// while the real database stays untouched (migrations/README.md);
		// "docker compose -f docker-compose.yml up -d" starts Traefik, which
		// is wrong because DSM already holds port 80
		// (docs/specs/01-architecture-and-deployment.md, "Synology NAS
		// variant"). README.md names the right two commands for every
		// variant.
		return fmt.Sprintf(`Database schema is %d migration%s behind this binary.
Run:  apply the pending migrations (migrate up) the way you deploy it (see README.md).
Then: start the stack the same way.`, e.Pending, plural(e.Pending))
	}
	return fmt.Sprintf(`Database schema (version %d) is newer than this binary (version %d).
This image is older than the database it was pointed at — a restored dump from a
newer release, or a rolled-back image. Migrations are forward-only: there is no
down path to run.
Deploy the matching (newer) image, or restore the backup taken before the upgrade
(docs/specs/15-backup-restore-and-export.md).`, e.DBVersion, e.BinaryVersion)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Check compares the database's goose version against the migrations this
// binary ships, and returns *SchemaMismatchError when they disagree.
//
// It is called by `serve` before the listener starts
// (docs/specs/18-operations-and-observability.md): skipping step 3 of the
// upgrade procedure should be loud rather than weird. Without it, a binary
// running against an older schema answers requests until the first query that
// touches a missing column, and reports that as a 500 with a driver message
// nobody can act on.
//
// # It is read-only, deliberately
//
// The version is read with a plain SELECT rather than goose's own
// GetDBVersion, which calls EnsureDBVersion and *creates* the bookkeeping
// table when it is absent. A startup check that writes to the schema it is
// checking is a check that changed the thing it measured; on a fresh install
// it would also leave a goose table behind after refusing to start.
//
// # What is not an error here
//
// A database that cannot be reached is returned as an ordinary error, not a
// SchemaMismatchError. That distinction is the whole point of the typed error:
// "unreachable" is worth retrying and gets the generic exit code the restart
// policy will keep retrying, while "wrong schema" is not and gets the distinct
// non-retryable one (cmd/inventory/main.go).
func Check(ctx context.Context, dsn string) error {
	dir, err := resolveDir()
	if err != nil {
		return err
	}

	shipped, err := shippedVersions(dir)
	if err != nil {
		return err
	}
	if len(shipped) == 0 {
		// No migrations in the image at all. Run() already treats this as a
		// packaging state rather than a failure, and refusing to start over it
		// would make an empty migrations directory fatal for no gain.
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

	binaryVersion := shipped[len(shipped)-1]
	pending := 0
	for _, version := range shipped {
		if !applied[version] {
			pending++
		}
	}

	if pending > 0 || highest > binaryVersion {
		return &SchemaMismatchError{DBVersion: highest, BinaryVersion: binaryVersion, Pending: pending}
	}
	return nil
}

// appliedVersions returns the set of migrations the database currently has
// applied, and the highest of them (0 when it has never been migrated).
//
// # Why a set rather than one number
//
// goose records a rollback by appending a row with is_applied = false rather
// than deleting the one that recorded the apply, so the table is a log and the
// *last row per version* is that version's current state. Reading "the highest
// version_id" would report a rolled-back migration as applied; reading "the
// highest applied version" would miss a hole — version 3 applied with version
// 2 rolled back underneath it. Comparing the shipped list against the whole
// set catches both, and needs no assumption about the order migrations were
// applied in.
func appliedVersions(ctx context.Context, db *sql.DB) (map[int64]bool, int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT version_id
		  FROM (
		       SELECT DISTINCT ON (version_id) version_id, is_applied
		         FROM goose_db_version
		        ORDER BY version_id, id DESC
		       ) current
		 WHERE is_applied`)
	if err != nil {
		if isUndefinedTable(err) {
			// Never migrated: goose's bookkeeping table does not exist yet.
			return map[int64]bool{}, 0, nil
		}
		return nil, 0, fmt.Errorf("migrate: read schema version: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[int64]bool{}
	var highest int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, 0, fmt.Errorf("migrate: read schema version: %w", err)
		}
		applied[version] = true
		if version > highest {
			highest = version
		}
	}
	if err := rows.Err(); err != nil {
		if isUndefinedTable(err) {
			return map[int64]bool{}, 0, nil
		}
		return nil, 0, fmt.Errorf("migrate: read schema version: %w", err)
	}
	return applied, highest, nil
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == undefinedTable
}

// shippedVersions lists the migration versions in dir, ascending.
//
// Versions come from goose.NumericComponent, the same parser goose applies to
// the same filenames, so the check cannot disagree with the runner about what
// "migration 10" is. A file goose would reject is skipped rather than fatal:
// `migrate up` is where a malformed name should fail, loudly and with goose's
// own message, not here in a comparison that would then report a nonsense
// count.
func shippedVersions(dir string) ([]int64, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, fmt.Errorf("migrate: scan %s: %w", dir, err)
	}

	versions := make([]int64, 0, len(matches))
	for _, match := range matches {
		version, err := goose.NumericComponent(strings.TrimSpace(filepath.Base(match)))
		if err != nil {
			continue
		}
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions, nil
}
