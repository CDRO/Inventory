package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, for the throwaway-database helpers below
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chdir moves into dir for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()

	previous, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

// TestCountMigrations covers the check that decides whether Run touches the
// database at all. A broken glob here either skips real migrations silently or
// tries to connect when there is nothing to apply.
func TestCountMigrations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files []string
		want  int
	}{
		{name: "empty directory", files: nil, want: 0},
		{name: "only the README", files: []string{"README.md"}, want: 0},
		{name: "one migration", files: []string{"0001_init.sql"}, want: 1},
		{name: "several migrations", files: []string{"0001_init.sql", "0002_products.sql"}, want: 2},
		{name: "migrations alongside docs", files: []string{"README.md", "0001_init.sql"}, want: 1},
		{name: "sql suffix only, not substring", files: []string{"notes.sql.bak"}, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for _, name := range tc.files {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("-- x"), 0o600))
			}

			got, err := countMigrations(dir)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolveDirPrefersImagePath checks the candidate order. The production
// image keeps migrations at /migrations; the dev container has the repo
// bind-mounted and finds ./migrations instead.
func TestResolveDirPrefersImagePath(t *testing.T) {
	// Not parallel: chdir mutates process state.
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "migrations"), 0o755))
	chdir(t, root)

	dir, err := resolveDir()

	require.NoError(t, err)
	assert.Equal(t, "migrations", dir, "the relative candidate is used when /migrations is absent")
}

// TestResolveDirFailsLoudly makes a packaging mistake — an image built without
// its migrations — say so rather than reporting "nothing to apply" and leaving
// the schema silently unmigrated.
func TestResolveDirFailsLoudly(t *testing.T) {
	chdir(t, t.TempDir())

	_, err := resolveDir()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no migrations directory found")
}

// TestRunReportsNothingToApply covers the state this spec actually ships in:
// the runner exists, the schema does not. It must return cleanly without
// opening a database connection, so `migrate up` succeeds on a deployment that
// has no migrations yet.
func TestRunReportsNothingToApply(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "migrations"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "migrations", "README.md"), []byte("# soon"), 0o600))
	chdir(t, root)

	var out bytes.Buffer
	// A DSN that could not possibly connect: reaching the database at all
	// would be the bug this test is looking for.
	err := Run(context.Background(), "postgres://nowhere:1/none", "up", &out)

	require.NoError(t, err)
	assert.Contains(t, out.String(), "nothing to apply")
}

// TestRunRejectsUnknownAction keeps a typo from being reported as success.
func TestRunRejectsUnknownAction(t *testing.T) {
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(migrationsDir, "0001_init.sql"),
		[]byte("-- +goose Up\nSELECT 1;\n"),
		0o600,
	))
	chdir(t, root)

	var out bytes.Buffer
	err := Run(context.Background(), "postgres://user:pw@127.0.0.1:1/none", "sideways", &out)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown action "sideways"`)
}

// requireAdminDSN skips a test when no database is configured, matching the
// same pattern internal/store's tests use: `go test ./...` inside the
// Dockerfile builder stage has no database, and only
// `docker compose run --rm app go test ./...` sets DATABASE_URL.
func requireAdminDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; run via `docker compose run --rm app go test ./...`")
	}
	return dsn
}

// newTestDatabase creates a throwaway database on the same server as
// DATABASE_URL and returns its DSN, dropping it when the test ends. Every
// test below gets its own — these tests apply real, sometimes deliberately
// broken, migrations, which the store package's shared, pre-migrated
// testStore/testPool cannot represent.
func newTestDatabase(t *testing.T) string {
	t.Helper()

	adminDSN := requireAdminDSN(t)

	admin, err := sql.Open("pgx", adminDSN)
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()

	name := "migrate_test_" + randomSuffix()
	// CREATE DATABASE cannot run inside a transaction, and the name is
	// generated here rather than supplied, so quoting it is enough.
	_, err = admin.Exec(fmt.Sprintf(`CREATE DATABASE %q`, name))
	require.NoError(t, err)

	t.Cleanup(func() {
		drop, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		// FORCE terminates any connection still attached, so a leaked pool
		// cannot leave the throwaway database behind.
		_, _ = drop.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	})

	parsed, err := url.Parse(adminDSN)
	require.NoError(t, err)
	parsed.Path = "/" + name
	return parsed.String()
}

func randomSuffix() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("migrate tests: no entropy: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func writeMigration(t *testing.T, dir, name, upSQL, downSQL string) {
	t.Helper()

	content := "-- +goose Up\n" + upSQL + "\n-- +goose Down\n" + downSQL + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

// TestRunUpStaysNonNoisyOnHappyPath is the regression for a bug review-tests
// caught in this PR's first round: making status/version print by forwarding
// every goose Printf call unconditionally also forwarded goose's own
// per-migration "OK <file>" lines and its "successfully migrated" line
// during `up`, on top of the app's existing one-line summary — acceptance
// criterion "migrate up output stays useful without becoming noisy on the
// happy path", violated. fatalLogger's Printf is now silent unless the
// action is status/version, so `up` must still produce exactly the one line
// it always has.
func TestRunUpStaysNonNoisyOnHappyPath(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigration(t, migrationsDir, "00001_first.sql", "CREATE TABLE t1 (id int);", "DROP TABLE t1;")
	writeMigration(t, migrationsDir, "00002_second.sql", "CREATE TABLE t2 (id int);", "DROP TABLE t2;")
	chdir(t, root)

	var out bytes.Buffer
	err := Run(context.Background(), dsn, "up", &out)
	require.NoError(t, err)

	assert.Equal(t, "Applied migrations from migrations.\n", out.String(),
		"up must print exactly its own one-line summary, not goose's internal per-migration Printf output")
}

// TestRunStatusListsAppliedAndPendingMigrations is the regression for the
// defect issue #24 tracks: `migrate status` used to print nothing at all,
// because goose reports status through a Logger the application set to
// goose.NopLogger. Applies two migrations, adds a third afterward without
// applying it, and checks status reports all three — two applied, one
// pending — rather than silence.
func TestRunStatusListsAppliedAndPendingMigrations(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigration(t, migrationsDir, "00001_first.sql", "CREATE TABLE t1 (id int);", "DROP TABLE t1;")
	writeMigration(t, migrationsDir, "00002_second.sql", "CREATE TABLE t2 (id int);", "DROP TABLE t2;")
	chdir(t, root)

	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))

	// Added after `up` ran, so it stays pending.
	writeMigration(t, migrationsDir, "00003_third.sql", "CREATE TABLE t3 (id int);", "DROP TABLE t3;")

	var out bytes.Buffer
	err := Run(context.Background(), dsn, "status", &out)
	require.NoError(t, err)

	got := out.String()
	assert.NotEmpty(t, got, "status must print something, not silently succeed")
	assert.Contains(t, got, "00001_first.sql")
	assert.Contains(t, got, "00002_second.sql")
	assert.Contains(t, got, "00003_third.sql")
	assert.Equal(t, 1, strings.Count(got, "Pending"), "exactly the third, unapplied migration should show Pending")
}

// TestRunVersionReportsCurrentVersion is the version half of the same
// regression: `migrate version` used to print nothing for the same reason.
func TestRunVersionReportsCurrentVersion(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigration(t, migrationsDir, "00001_first.sql", "CREATE TABLE t1 (id int);", "DROP TABLE t1;")
	chdir(t, root)

	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))

	var out bytes.Buffer
	err := Run(context.Background(), dsn, "version", &out)
	require.NoError(t, err)

	assert.Contains(t, out.String(), "goose: version 1")
}

// TestRunUpFailureReturnsErrorNotSuccess is the acceptance criterion issue
// #24 calls out by name: a failing `migrate up` must still return a non-zero
// exit code (cmd/inventory turns any non-nil Run error into one) and surface
// the underlying error, rather than the Fatalf-swallowing trap a naive fix
// (a Logger that only prints from Fatalf) would reintroduce.
func TestRunUpFailureReturnsErrorNotSuccess(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigration(t, migrationsDir, "00001_broken.sql", "THIS IS NOT VALID SQL;", "SELECT 1;")
	chdir(t, root)

	var out bytes.Buffer
	err := Run(context.Background(), dsn, "up", &out)

	require.Error(t, err, "a failing migration must not be reported as success")
	assert.NotContains(t, out.String(), "Applied migrations from",
		"the success message must not appear when the migration actually failed")
}

// TestRecoverFatalConvertsFatalfPanicToError exercises the panic/recover
// boundary directly, since no call in the pinned goose version (v3.22.1)
// actually reaches Logger.Fatalf — Up, Status and Version all fail through
// returned errors instead, so Run's public behavior alone cannot exercise
// this path. Guards against a future goose upgrade reintroducing a Fatalf
// call: if that happens, this is what stands between a real failure and a
// process that reports success because nothing panicked visibly.
func TestRecoverFatalConvertsFatalfPanicToError(t *testing.T) {
	var buf bytes.Buffer
	logger := &fatalLogger{out: &buf}

	got := func() (err error) {
		defer recoverFatal(&err)
		logger.Fatalf("boom: %s", "reason")
		return nil
	}()

	require.Error(t, got)
	assert.Contains(t, got.Error(), "boom: reason")
}

// TestRecoverFatalReraisesOtherPanics guards the type assertion in
// recoverFatal: it must only ever intercept the one panic shape this package
// produces, never mask an unrelated panic (a nil pointer dereference, an
// index out of range) as a clean migrate error.
func TestRecoverFatalReraisesOtherPanics(t *testing.T) {
	defer func() {
		r := recover()
		assert.Equal(t, "unrelated panic", r)
	}()

	func() (err error) {
		defer recoverFatal(&err)
		panic("unrelated panic")
	}()
}

// TestRunDownRollsBackTheRepositorysOwnMigrations is the rollback half of
// check_test.go's TestCheckAcceptsTheRepositorysOwnMigrations, and the only
// place any real `-- +goose Down` block under migrations/ is ever executed.
//
// Every other test in this package writes its own migration files, and both
// internal/store's test-database helper and that end-to-end guard stop at
// `up`. So without this test a typo in a down block — a misspelled column, the
// wrong table, a DROP naming something the up block never created — ships
// green and is never contradicted by anything.
//
// Where that surfaces is *not* a production rollback:
// docs/specs/18-operations-and-observability.md forecloses `migrate down` in
// production outright — the backup taken before an upgrade is the rollback,
// "because down-migrations against real data are tested never and trusted
// always". This test narrows that "tested never" by one migration. What it
// protects is the dev and staging use of `migrate down`, and the maintainer who
// reads a down block and assumes it works.
//
// The assertion is on storage_members.start_page, the column
// migrations/00013_storage_member_start_page.sql adds, whose acceptance
// criterion in docs/specs/34-navigation-and-start-page.md is "its down
// migration drops the column". `migrate up` afterwards must put it back: a
// rollback that cannot be undone is not a rollback.
//
// The rollback walks the shipped migration list from the newest version down to
// startPageVersion rather than naming a fixed number of steps, so it keeps
// covering the newest down block as the schema grows. **Today 00013 *is* the
// newest, so that walk is a single step** — the loop is written for the schema
// this test will live in, not for the one it has now, and there is currently no
// version above 13 at which to observe the column still present.
func TestRunDownRollsBackTheRepositorysOwnMigrations(t *testing.T) {
	// Not parallel: chdir mutates process state.
	ctx := context.Background()
	dsn := newTestDatabase(t)
	chdir(t, filepath.Join("..", ".."))

	// The version that introduced the column asserted below. Raise this
	// together with the column when a later migration replaces it.
	const startPageVersion int64 = 13

	versions, err := shippedVersions("migrations")
	require.NoError(t, err)
	require.NotEmpty(t, versions, "the repository ships migrations")
	newest := versions[len(versions)-1]
	require.GreaterOrEqual(t, newest, startPageVersion,
		"migration %d is shipped, so the newest version cannot be below it", startPageVersion)

	require.NoError(t, Run(ctx, dsn, "up", io.Discard))
	require.True(t, columnExists(t, dsn, "storage_members", "start_page"),
		"migrate up must add the column migration %d declares", startPageVersion)

	// One call per *shipped migration* from the newest down to
	// startPageVersion inclusive — iterating the versions slice rather than
	// counting integers down from newest. Run's "down" rolls back exactly one
	// applied migration, so an integer range would fire the wrong number of
	// calls the moment migration numbering has a gap in it: the assertion
	// below would still pass, having rolled back past the migration under
	// test, and the diagnostic would name a version that was never the target
	// of that call.
	for i := len(versions) - 1; i >= 0 && versions[i] >= startPageVersion; i-- {
		require.NoError(t, Run(ctx, dsn, "down", io.Discard),
			"rolling back migration %d", versions[i])
	}

	assert.False(t, columnExists(t, dsn, "storage_members", "start_page"),
		"migration %d's down block must drop start_page", startPageVersion)

	require.NoError(t, Run(ctx, dsn, "up", io.Discard))
	assert.True(t, columnExists(t, dsn, "storage_members", "start_page"),
		"re-applying must restore the column")
	assert.NoError(t, Check(ctx, dsn),
		"a database rolled back and migrated up again must satisfy the shipped binary")
}

// columnExists asks the database's own catalogue whether public.table.column
// is there.
//
// Reading the catalogue rather than goose_db_version is the whole point: the
// bookkeeping table records that goose *ran* a down block, not that the block
// did what it claims. Only information_schema can tell an ALTER that took
// effect from one that was recorded and did nothing.
func columnExists(t *testing.T, dsn, table, column string) bool {
	t.Helper()

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var exists bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (
		       SELECT 1
		         FROM information_schema.columns
		        WHERE table_schema = 'public'
		          AND table_name   = $1
		          AND column_name  = $2)`,
		table, column).Scan(&exists))
	return exists
}
