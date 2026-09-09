package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// testStore is the shared store for this package's database-backed tests. It
// points at a disposable database created in TestMain and dropped afterwards —
// never the production data in the pgdata volume
// (docs/specs/04-backend-api-conventions.md).
var testStore *store.Store

// testPool is a raw connection to the same throwaway database, used to build
// fixtures and to assert against the tables directly.
//
// Deliberately separate from testStore: a fixture written through the very
// writer under test would hide that writer being broken, and an assertion made
// through its reader would hide a reader that filters away the row it should
// have found. The tests below therefore set up and check with plain SQL, and
// use the store only for the operation being exercised.
var testPool *pgxpool.Pool

// TestMain creates one throwaway database for the whole package, migrates it,
// and drops it at the end.
//
// One database rather than one per test because every table here is
// storage-scoped: giving each test its own storages isolates them completely
// while costing one CREATE DATABASE instead of dozens.
//
// When DATABASE_URL is unset the database-backed tests skip. That is what lets
// `go test ./...` run inside the Dockerfile builder stage, which has no
// database — the pure-logic tests still run and still gate the image. Under
// `docker compose run --rm app go test ./...` the variable is set and these
// tests execute.
func TestMain(m *testing.M) {
	adminDSN := os.Getenv("DATABASE_URL")
	if adminDSN == "" {
		os.Exit(m.Run())
	}

	dbName := "inventory_test_" + randomSuffix()

	cleanup, dsn, err := createTestDatabase(adminDSN, dbName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store tests: prepare database: %v\n", err)
		os.Exit(1)
	}

	if err := migrateTestDatabase(dsn); err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "store tests: migrate: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	testStore, err = store.Open(ctx, dsn)
	if err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "store tests: open: %v\n", err)
		os.Exit(1)
	}

	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		testStore.Close()
		cleanup()
		fmt.Fprintf(os.Stderr, "store tests: open raw pool: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	testPool.Close()
	testStore.Close()
	cleanup()
	os.Exit(code)
}

// execTest runs a fixture statement against the throwaway database.
func execTest(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := testPool.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// queryCount runs a single-value count query against the throwaway database.
func queryCount(ctx context.Context, sql string, args ...any) (int, error) {
	var n int
	if err := testPool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func randomSuffix() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("store tests: no entropy: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// createTestDatabase issues CREATE DATABASE on the server named by adminDSN and
// returns a teardown plus the DSN of the new database.
func createTestDatabase(adminDSN, dbName string) (func(), string, error) {
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return nil, "", fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = admin.Close() }()

	// CREATE DATABASE cannot run inside a transaction, and the name is
	// generated here rather than supplied, so quoting it is enough.
	if _, err := admin.Exec(fmt.Sprintf(`CREATE DATABASE %q`, dbName)); err != nil {
		return nil, "", fmt.Errorf("create database: %w", err)
	}

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		return nil, "", fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	parsed.Path = "/" + dbName
	dsn := parsed.String()

	cleanup := func() {
		drop, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		// FORCE terminates any connection still attached, so a leaked pool
		// cannot leave the throwaway database behind.
		_, _ = drop.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, dbName))
	}
	return cleanup, dsn, nil
}

// migrateTestDatabase applies the real migrations, so these tests exercise the
// schema that ships rather than a hand-written copy that could drift from it.
func migrateTestDatabase(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("dialect: %w", err)
	}
	goose.SetLogger(goose.NopLogger())

	if err := goose.Up(db, "../../migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// requireDB skips a test when no database is configured.
func requireDB(t *testing.T) *store.Store {
	t.Helper()

	if testStore == nil {
		t.Skip("DATABASE_URL not set; run via `docker compose run --rm app go test ./...`")
	}
	return testStore
}

// newStorage inserts a storage and returns its id. Each test gets its own, so
// tests cannot see each other's rows even though they share a database.
func newStorage(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()

	id, err := uuid.NewV7()
	require.NoError(t, err)

	_, err = execTest(ctx, `INSERT INTO storages (id, name) VALUES ($1, $2)`, id, "test-"+id.String()[:8])
	require.NoError(t, err)
	return id
}

// newUser inserts a user and returns its id.
func newUser(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()

	id, err := uuid.NewV7()
	require.NoError(t, err)

	_, err = execTest(ctx,
		`INSERT INTO users (id, username, password_hash, display_name) VALUES ($1, $2, 'x', $3)`,
		id, "u-"+id.String(), "Test User")
	require.NoError(t, err)
	return id
}

// countRows is a small assertion helper for the invariant tests.
func countRows(t *testing.T, ctx context.Context, sql string, args ...any) int {
	t.Helper()

	n, err := queryCount(ctx, sql, args...)
	require.NoError(t, err)
	return n
}

func day(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}

func ptrString(s string) *string { return &s }

func ptrInt(i int) *int { return &i }

// newUUID returns an id that names no row, for the "nonexistent" half of the
// indistinguishability tests.
func newUUID(t *testing.T) uuid.UUID {
	t.Helper()

	id, err := uuid.NewV7()
	require.NoError(t, err)
	return id
}

// timeNow reads the clock from the database rather than the test process, so
// comparisons line up with the now() that DEFAULT clauses use. A container and
// its database can disagree by enough to make a boundary test flap.
func timeNow(t *testing.T, ctx context.Context) time.Time {
	t.Helper()

	var now time.Time
	require.NoError(t, testPool.QueryRow(ctx, `SELECT now()`).Scan(&now))
	return now
}

// timeZero is a cursor older than any row, standing in for a client that has
// been offline longer than the tombstone retention window.
func timeZero() time.Time {
	return time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
}
