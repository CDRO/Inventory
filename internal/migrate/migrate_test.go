package migrate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

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
