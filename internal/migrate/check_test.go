package migrate

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMigrations creates a migrations directory holding the given versions
// and chdirs into its parent, so resolveDir finds it.
//
// Real files rather than a fake directory listing: Check parses them with
// goose.NumericComponent, and a test that handed it version numbers directly
// would not notice the day the project's filenames and goose's parser stop
// agreeing.
func writeMigrations(t *testing.T, names ...string) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(dir, 0o755))
	for _, name := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name),
			[]byte("-- +goose Up\nSELECT 1;\n"), 0o600))
	}
	chdir(t, root)
}

// applyThrough runs goose up against dsn for the migrations currently in
// ./migrations, so the database's recorded version is whatever those files say.
func applyThrough(t *testing.T, dsn string) {
	t.Helper()
	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))
}

// TestCheckPassesWhenTheSchemaMatchesTheBinary — the happy path, and the one
// every `docker compose up -d` after a correct upgrade takes.
func TestCheckPassesWhenTheSchemaMatchesTheBinary(t *testing.T) {
	// Not parallel: chdir mutates process state.
	dsn := newTestDatabase(t)
	writeMigrations(t, "00001_first.sql", "00002_second.sql")
	applyThrough(t, dsn)

	assert.NoError(t, Check(context.Background(), dsn))
}

// TestCheckRefusesADatabaseBehindTheBinary is the acceptance criterion: `serve`
// refuses to start, with the documented message, when migrations are pending.
//
// The count and both commands are asserted because the message *is* the
// remediation — an operator reading `docker compose logs app` has nothing else
// to go on, and "schema mismatch" without the command to fix it is a message
// that makes somebody open a search engine.
func TestCheckRefusesADatabaseBehindTheBinary(t *testing.T) {
	dsn := newTestDatabase(t)

	// One migration applied…
	writeMigrations(t, "00001_first.sql")
	applyThrough(t, dsn)

	// …then the binary grows two more.
	writeMigrations(t, "00001_first.sql", "00002_second.sql", "00003_third.sql")

	err := Check(context.Background(), dsn)

	var mismatch *SchemaMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.True(t, mismatch.Behind())
	assert.Equal(t, 2, mismatch.Pending)
	assert.Equal(t, int64(1), mismatch.DBVersion)
	assert.Equal(t, int64(3), mismatch.BinaryVersion)

	message := err.Error()
	assert.Contains(t, message, "Database schema is 2 migrations behind this binary.")
	assert.Contains(t, message, "docker compose -f docker-compose.yml run --rm app migrate up")
	assert.Contains(t, message, "docker compose -f docker-compose.yml up -d")
}

// TestCheckRefusesADatabaseAheadOfTheBinary — a restored newer dump, or a
// rolled-back image.
//
// Named rather than left to whatever query first meets an unknown column:
// migrations are forward-only, so there is nothing the binary can do about it
// and the message has to say what a person should do instead.
func TestCheckRefusesADatabaseAheadOfTheBinary(t *testing.T) {
	dsn := newTestDatabase(t)

	writeMigrations(t, "00001_first.sql", "00002_second.sql", "00003_third.sql")
	applyThrough(t, dsn)

	// The same database, an older binary.
	writeMigrations(t, "00001_first.sql")

	err := Check(context.Background(), dsn)

	var mismatch *SchemaMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.False(t, mismatch.Behind())
	assert.Equal(t, int64(3), mismatch.DBVersion)
	assert.Equal(t, int64(1), mismatch.BinaryVersion)
	assert.Contains(t, err.Error(), "newer than this binary")
	assert.Contains(t, err.Error(), "forward-only")
}

// TestCheckTreatsANeverMigratedDatabaseAsBehindEverything.
//
// A fresh install has no goose bookkeeping table at all, and reading it raises
// "relation does not exist" rather than returning zero. Getting that wrong
// means the very first `docker compose up -d` fails with a driver error
// instead of the message naming `migrate up` — the exact situation this check
// exists for.
func TestCheckTreatsANeverMigratedDatabaseAsBehindEverything(t *testing.T) {
	dsn := newTestDatabase(t)
	writeMigrations(t, "00001_first.sql", "00002_second.sql")

	err := Check(context.Background(), dsn)

	var mismatch *SchemaMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, int64(0), mismatch.DBVersion)
	assert.Equal(t, 2, mismatch.Pending)
}

// TestCheckDoesNotCreateGooseBookkeeping — the check is read-only.
//
// goose's own GetDBVersion calls EnsureDBVersion and creates the table as a
// side effect. A startup check that did that would write to the schema it just
// refused to run against, and leave a table behind on a deployment that never
// started.
func TestCheckDoesNotCreateGooseBookkeeping(t *testing.T) {
	dsn := newTestDatabase(t)
	writeMigrations(t, "00001_first.sql")

	require.Error(t, Check(context.Background(), dsn))

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var exists bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&exists))
	assert.False(t, exists, "the check must not have created goose's table")
}

// TestCheckIgnoresARolledBackMigrationsHighestNumber.
//
// goose records a rollback as a new row with is_applied = false rather than
// deleting the old one, so "the highest version_id in the table" is not the
// current version. Taking max() would report a rolled-back migration as
// applied and let a binary start against a schema that is missing it.
func TestCheckIgnoresARolledBackMigrationsHighestNumber(t *testing.T) {
	dsn := newTestDatabase(t)
	writeMigrations(t, "00001_first.sql", "00002_second.sql")
	applyThrough(t, dsn)

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (2, false)`)
	require.NoError(t, err)

	err = Check(context.Background(), dsn)

	var mismatch *SchemaMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, int64(1), mismatch.DBVersion, "the rolled-back migration is not the current version")
	assert.Equal(t, 1, mismatch.Pending)
}

// TestCheckOnAnUnreachableDatabaseIsNotASchemaMismatch.
//
// The distinction is the point of the typed error: a schema mismatch is fatal
// and non-retryable and gets the distinct exit code, while an unreachable
// database is worth retrying and must not be reported as "run migrate up",
// which would send an operator to fix the wrong thing.
func TestCheckOnAnUnreachableDatabaseIsNotASchemaMismatch(t *testing.T) {
	writeMigrations(t, "00001_first.sql")

	err := Check(context.Background(), "postgres://nobody:nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1")

	require.Error(t, err)
	var mismatch *SchemaMismatchError
	assert.False(t, errors.As(err, &mismatch),
		"an unreachable database is not a schema mismatch")
	assert.NotContains(t, err.Error(), "migrate up",
		"and must not send the operator to fix the wrong thing")
}

// TestCheckWithNoMigrationsInTheImageIsNotAFailure — Run already treats an
// empty migrations directory as a packaging state rather than an error, and
// refusing to start over it would gain nothing.
func TestCheckWithNoMigrationsInTheImageIsNotAFailure(t *testing.T) {
	dsn := newTestDatabase(t)
	writeMigrations(t)

	assert.NoError(t, Check(context.Background(), dsn))
}

// TestShippedVersionsSkipsFilesGooseWouldNotAccept.
//
// A malformed name must fail in `migrate up`, with goose's own message, not
// here — a check that counted it would report a pending-migration count that
// no `migrate up` could ever satisfy.
func TestShippedVersionsSkipsFilesGooseWouldNotAccept(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(dir, 0o755))
	for _, name := range []string{"00001_first.sql", "notanumber.sql", "00002_second.sql"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("-- x"), 0o600))
	}

	versions, err := shippedVersions(dir)

	require.NoError(t, err)
	assert.Equal(t, []int64{1, 2}, versions)
}

// TestCheckAcceptsTheRepositorysOwnMigrations is the end-to-end guard the
// synthetic cases above cannot give.
//
// Every other test in this file writes its own migration files, so all of them
// would keep passing if the real ones were named in a way goose's parser reads
// differently from this package's — and the symptom would be a production
// server refusing to start against a database that is perfectly up to date,
// with a message telling the operator to run a migration that has already run.
// This applies the actual migrations/ directory and then asks Check about it.
func TestCheckAcceptsTheRepositorysOwnMigrations(t *testing.T) {
	// Not parallel: chdir mutates process state.
	dsn := newTestDatabase(t)
	chdir(t, filepath.Join("..", ".."))

	versions, err := shippedVersions("migrations")
	require.NoError(t, err)
	require.NotEmpty(t, versions, "the repository ships migrations")
	require.Equal(t, int64(len(versions)), versions[len(versions)-1],
		"versions are 1..N with no gaps, so a count is a version")

	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))

	assert.NoError(t, Check(context.Background(), dsn),
		"a database migrated with the shipped files must satisfy the shipped binary")
}
