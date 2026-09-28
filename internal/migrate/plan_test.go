package migrate

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMigrationBytes writes a migration file verbatim, for the tests below
// that need exact control over line breaks — writeMigration (migrate_test.go)
// always joins with "\n", which cannot represent a CRLF fixture.
func writeMigrationBytes(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), content, 0o600))
}

// TestPlanReportsNothingPendingWhenFullyMigrated is the acceptance criterion's
// "nothing pending" case: exit 0, and the exact line with no parenthetical and
// no file lines — not a "(0 pending: )" rendering of rolling.
func TestPlanReportsNothingPendingWhenFullyMigrated(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigration(t, migrationsDir, "00001_first.sql", "CREATE TABLE t1 (id int);", "DROP TABLE t1;")
	writeMigration(t, migrationsDir, "00002_second.sql", "CREATE TABLE t2 (id int);", "DROP TABLE t2;")
	chdir(t, root)

	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))

	var out bytes.Buffer
	err := Plan(context.Background(), dsn, &out)

	require.NoError(t, err)
	assert.Equal(t, "migrate plan: nothing pending\n", out.String())
}

// TestPlanReportsRollingForPendingMigrationsWithoutTheMarker covers the
// ordinary case: pending migrations, none of which need a classic deploy.
func TestPlanReportsRollingForPendingMigrationsWithoutTheMarker(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigration(t, migrationsDir, "00001_first.sql", "CREATE TABLE t1 (id int);", "DROP TABLE t1;")
	chdir(t, root)
	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))

	writeMigration(t, migrationsDir, "00002_second.sql", "CREATE TABLE t2 (id int);", "DROP TABLE t2;")
	writeMigration(t, migrationsDir, "00003_third.sql", "CREATE TABLE t3 (id int);", "DROP TABLE t3;")

	var out bytes.Buffer
	err := Plan(context.Background(), dsn, &out)

	require.NoError(t, err)
	got := out.String()
	assert.Contains(t, got, "migrate plan: rolling (2 pending: 00002_second.sql, 00003_third.sql)\n")
	assert.Contains(t, got, "00002_second.sql")
	assert.Contains(t, got, "00003_third.sql")
}

// TestPlanReportsClassicWhenAPendingMigrationCarriesTheMarker is the spec's
// other ordinary outcome: at least one pending migration needs a classic
// deploy, reported with the spec's distinct exit code (via *PlanClassicError,
// which cmd/inventory maps to ExitClassic).
func TestPlanReportsClassicWhenAPendingMigrationCarriesTheMarker(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigration(t, migrationsDir, "00001_first.sql", "CREATE TABLE t1 (id int);", "DROP TABLE t1;")
	chdir(t, root)
	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))

	writeMigration(t, migrationsDir, "00002_second.sql", "CREATE TABLE t2 (id int);", "DROP TABLE t2;")
	writeMigrationBytes(t, migrationsDir, "00003_classic.sql",
		[]byte("-- +goose Up\n-- +inventory:classic\nALTER TABLE t1 DROP COLUMN id;\n-- +goose Down\nSELECT 1;\n"))

	var out bytes.Buffer
	err := Plan(context.Background(), dsn, &out)

	var classicErr *PlanClassicError
	require.ErrorAs(t, err, &classicErr)
	got := out.String()
	assert.Contains(t, got, "migrate plan: classic (2 pending: 00002_second.sql, 00003_classic.sql)\n")
	assert.Contains(t, got, "00003_classic.sql")
}

// TestPlanIgnoresAMarkerInAnAlreadyAppliedMigration is decision D3's rule that
// the marker only matters in pending files: once a migration has run, its
// marker — well-formed or not — changes nothing, because the deploy it would
// have gated already happened.
func TestPlanIgnoresAMarkerInAnAlreadyAppliedMigration(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigrationBytes(t, migrationsDir, "00001_classic.sql",
		[]byte("-- +goose Up\n-- +inventory:classic\nCREATE TABLE t1 (id int);\n-- +goose Down\nDROP TABLE t1;\n"))
	chdir(t, root)
	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))

	writeMigration(t, migrationsDir, "00002_second.sql", "CREATE TABLE t2 (id int);", "DROP TABLE t2;")

	var out bytes.Buffer
	err := Plan(context.Background(), dsn, &out)

	require.NoError(t, err, "the applied migration's marker must not force a classic deploy")
	assert.Contains(t, out.String(), "migrate plan: rolling (1 pending: 00002_second.sql)\n")
}

// TestPlanRejectsAMalformedMarkerInThePendingFile is the acceptance
// criterion's "a marker that differs only in spacing or trailing text" case:
// rejected as an error, not accepted loosely and not read as no marker.
func TestPlanRejectsAMalformedMarkerInThePendingFile(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	chdir(t, root)

	writeMigrationBytes(t, migrationsDir, "00001_malformed.sql",
		[]byte("-- +goose Up\n-- +inventory:classic \nCREATE TABLE t1 (id int);\n-- +goose Down\nDROP TABLE t1;\n"))

	var out bytes.Buffer
	err := Plan(context.Background(), dsn, &out)

	var placement *MarkerPlacementError
	require.ErrorAs(t, err, &placement)
	assert.Equal(t, 2, placement.Line)
	assert.Empty(t, out.String(), "no report is printed once a marker error is found")
}

// TestPlanRejectsAMarkerOutsideTheGooseUpBlock covers the placement half:
// the exact marker line, but not the first non-blank line after
// "-- +goose Up".
func TestPlanRejectsAMarkerOutsideTheGooseUpBlock(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	chdir(t, root)

	writeMigrationBytes(t, migrationsDir, "00001_misplaced.sql",
		[]byte("-- +goose Up\nCREATE TABLE t1 (id int);\n-- +goose Down\n-- +inventory:classic\nDROP TABLE t1;\n"))

	var out bytes.Buffer
	err := Plan(context.Background(), dsn, &out)

	var placement *MarkerPlacementError
	require.ErrorAs(t, err, &placement)
	assert.Equal(t, 4, placement.Line)
}

// TestPlanAcceptsACRLFTerminatedMarker is the CRLF-tolerance acceptance
// criterion: the terminator is not part of the line, so a Windows checkout
// must not reject a marker it would otherwise accept.
func TestPlanAcceptsACRLFTerminatedMarker(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	chdir(t, root)

	writeMigrationBytes(t, migrationsDir, "00001_crlf.sql",
		[]byte("-- +goose Up\r\n-- +inventory:classic\r\nCREATE TABLE t1 (id int);\r\n-- +goose Down\r\nDROP TABLE t1;\r\n"))

	var out bytes.Buffer
	err := Plan(context.Background(), dsn, &out)

	var classicErr *PlanClassicError
	require.ErrorAs(t, err, &classicErr)
	assert.Contains(t, out.String(), "migrate plan: classic (1 pending: 00001_crlf.sql)\n")
}

// TestPlanTreatsADatabaseAheadOfTheBinaryAsFatal is spec 18's fatal, named
// error, reused rather than reinvented: a database that has applied a
// migration this binary does not ship is not something Plan can have an
// opinion about deploying.
func TestPlanTreatsADatabaseAheadOfTheBinaryAsFatal(t *testing.T) {
	dsn := newTestDatabase(t)
	root := t.TempDir()
	migrationsDir := filepath.Join(root, "migrations")
	require.NoError(t, os.Mkdir(migrationsDir, 0o755))
	writeMigration(t, migrationsDir, "00001_first.sql", "CREATE TABLE t1 (id int);", "DROP TABLE t1;")
	writeMigration(t, migrationsDir, "00002_second.sql", "CREATE TABLE t2 (id int);", "DROP TABLE t2;")
	writeMigration(t, migrationsDir, "00003_third.sql", "CREATE TABLE t3 (id int);", "DROP TABLE t3;")
	chdir(t, root)
	require.NoError(t, Run(context.Background(), dsn, "up", io.Discard))

	// The same database, an older binary — the exact fixture
	// TestCheckRefusesADatabaseAheadOfTheBinary (check_test.go) uses.
	require.NoError(t, os.Remove(filepath.Join(migrationsDir, "00002_second.sql")))
	require.NoError(t, os.Remove(filepath.Join(migrationsDir, "00003_third.sql")))

	var out bytes.Buffer
	err := Plan(context.Background(), dsn, &out)

	var mismatch *SchemaMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.False(t, mismatch.Behind())
	assert.Equal(t, int64(3), mismatch.DBVersion)
	assert.Equal(t, int64(1), mismatch.BinaryVersion)
	assert.Empty(t, out.String(), "no report is printed once the database is ahead of the binary")
}

// TestParseClassicMarkerAcceptsTheExactLine is the plain happy path of the
// exported parser, with no file or database involved.
func TestParseClassicMarkerAcceptsTheExactLine(t *testing.T) {
	classic, err := ParseClassicMarker([]byte("-- +goose Up\n-- +inventory:classic\nSELECT 1;\n-- +goose Down\nSELECT 1;\n"))

	require.NoError(t, err)
	assert.True(t, classic)
}

// TestParseClassicMarkerSkipsBlankLinesBeforeTheMarker: "first non-blank
// line" means blank lines between "-- +goose Up" and the marker do not
// disqualify it.
func TestParseClassicMarkerSkipsBlankLinesBeforeTheMarker(t *testing.T) {
	classic, err := ParseClassicMarker([]byte("-- +goose Up\n\n\n-- +inventory:classic\nSELECT 1;\n"))

	require.NoError(t, err)
	assert.True(t, classic)
}

// TestParseClassicMarkerRejectsATypoInTheDirective covers a namespace
// collision this package owns entirely: any "-- +inventory:…" line that is
// not the exact marker is a mistake to report, not an unrelated comment to
// ignore, wherever it appears.
func TestParseClassicMarkerRejectsATypoInTheDirective(t *testing.T) {
	_, err := ParseClassicMarker([]byte("-- +goose Up\n-- +inventory:classi\nSELECT 1;\n"))

	var placement *MarkerPlacementError
	require.ErrorAs(t, err, &placement)
	assert.Equal(t, 2, placement.Line)
}

// TestParseClassicMarkerReturnsFalseWithoutTheNamespace is the ordinary
// negative case: a migration with no opinion about deploy mode at all, the
// overwhelming majority of them.
func TestParseClassicMarkerReturnsFalseWithoutTheNamespace(t *testing.T) {
	classic, err := ParseClassicMarker([]byte("-- +goose Up\nCREATE TABLE t (id int);\n-- +goose Down\nDROP TABLE t;\n"))

	require.NoError(t, err)
	assert.False(t, classic)
}
