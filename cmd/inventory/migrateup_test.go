package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, for the throwaway-database helpers below
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunMigrateUpImportsTheIconLibrary is the wiring test review-tests asked
// for on this PR: ImportIcons and bootstrapAdmin are each tested in
// isolation, but nothing proved that `inventory migrate up` — the actual
// command a fresh install runs — still calls both. A regression that deleted
// or reordered runMigrate's call to importIcons would leave every other test
// green while breaking spec 42's headline acceptance criterion ("a fresh
// install ends with a populated, searchable icons table after migrate up,
// with no separate manual step").
//
// This runs the real `run(["migrate", "up"])` entry point against a
// throwaway database — the same command `docker compose run --rm app
// migrate up` invokes — rather than calling runMigrate or importIcons
// directly, so a regression in run()'s own dispatch would fail it too.
func TestRunMigrateUpImportsTheIconLibrary(t *testing.T) {
	// Not parallel: chdir and t.Setenv both mutate process-wide state.
	dsn := newTestDatabase(t)
	chdir(t, filepath.Join("..", ".."))

	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SESSION_SECRET", "test-session-secret")
	t.Setenv("GEMINI_API_KEY", "test-gemini-key")
	t.Setenv("ADMIN_INITIAL_PASSWORD", "test-admin-password")

	require.NoError(t, run([]string{"migrate", "up"}))

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()

	var userCount int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM users WHERE is_admin`).Scan(&userCount))
	assert.Equal(t, 1, userCount, "migrate up must also create the initial admin")

	var iconCount int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM icons WHERE source = 'vendored'`).Scan(&iconCount))
	assert.Greater(t, iconCount, 3000, "migrate up must also import the vendored icon library")

	var name string
	require.NoError(t, admin.QueryRow(`SELECT name FROM icons WHERE name = 'noto:cheese-wedge'`).Scan(&name))
	assert.Equal(t, "noto:cheese-wedge", name)

	// Re-running must change nothing and must not error — the acceptance
	// criterion `inventory icons import` itself rests on, exercised here
	// through the same `migrate up` entry point.
	require.NoError(t, run([]string{"migrate", "up"}))

	var iconCountAfter int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM icons WHERE source = 'vendored'`).Scan(&iconCountAfter))
	assert.Equal(t, iconCount, iconCountAfter, "re-running migrate up must insert no duplicate icons")
}

// chdir moves into dir for the duration of the test, the same helper
// internal/migrate/migrate_test.go uses: run()'s own migrate.Run resolves
// the migrations directory relative to the working directory
// (candidateDirs in internal/migrate/migrate.go), which only exists at the
// repository root, not inside cmd/inventory.
func chdir(t *testing.T, dir string) {
	t.Helper()

	previous, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

// newTestDatabase creates a throwaway, unmigrated database on the same
// server as DATABASE_URL and returns its DSN, dropping it when the test
// ends — the same pattern internal/store and internal/migrate's own tests
// use, duplicated here because it is a small, self-contained helper and
// those packages' copies are unexported.
func newTestDatabase(t *testing.T) string {
	t.Helper()

	adminDSN := os.Getenv("DATABASE_URL")
	if adminDSN == "" {
		t.Skip("DATABASE_URL not set; run via `docker compose run --rm app go test ./...`")
	}

	admin, err := sql.Open("pgx", adminDSN)
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()

	name := "inventory_cmd_test_" + randomSuffix()
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
		panic("cmd/inventory tests: no entropy: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
