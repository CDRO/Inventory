package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/config"
)

// TestExitCodeForConfigFailure is the guard on the "fatal, non-retryable exit
// with a distinct code" rule. Docker restart policies do not look at exit
// codes, so this code is what tells an operator reading `docker compose logs
// app` that the container is misconfigured rather than crashing — and a
// regression to a generic 1 would erase that signal silently, with every test
// still green.
func TestExitCodeForConfigFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "missing configuration exits with EX_CONFIG",
			err:  &config.MissingError{Names: []string{"DATABASE_URL"}},
			want: config.ExitConfig,
		},
		{
			name: "a wrapped missing-configuration error still exits with EX_CONFIG",
			err:  fmt.Errorf("starting server: %w", &config.MissingError{Names: []string{"SESSION_SECRET"}}),
			want: config.ExitConfig,
		},
		{
			name: "any other failure exits generically",
			err:  errors.New("dial tcp: connection refused"),
			want: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, exitCodeFor(tc.err))
		})
	}

	assert.Equal(t, 78, config.ExitConfig, "EX_CONFIG is 78; changing it changes an operator-visible contract")
}

// TestErrorMessageKeepsRemediationUnprefixed checks that the remediation block
// reaches the operator intact. Prefixing it would push "No configuration
// found" behind noise on the line they actually read.
func TestErrorMessageKeepsRemediationUnprefixed(t *testing.T) {
	t.Parallel()

	missing := &config.MissingError{Names: []string{"DATABASE_URL"}}

	assert.Equal(t, missing.Error(), errorMessage(missing))
	assert.Equal(t, "inventory: boom", errorMessage(errors.New("boom")))
}

// TestRunRejectsUnknownCommand keeps a typo from being mistaken for `serve`,
// which would start a server the operator did not ask for.
func TestRunRejectsUnknownCommand(t *testing.T) {
	t.Parallel()

	err := run([]string{"migrat"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown command "migrat"`)
	assert.Equal(t, 1, exitCodeFor(err))
}

// TestStaticFSPrefersStaticDir covers the dev half of the asset switch: with
// STATIC_DIR set the server must read from disk, which is the whole reason
// editing a .js file and refreshing the browser is enough.
func TestStaticFSPrefersStaticDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>from disk</h1>"), 0o600))

	assets, err := staticFS(&config.Config{StaticDir: dir})
	require.NoError(t, err)

	got, err := fs.ReadFile(assets, "index.html")
	require.NoError(t, err)
	assert.Equal(t, "<h1>from disk</h1>", string(got))
}

// TestStaticFSFallsBackToEmbedded covers the production half: an empty
// STATIC_DIR must serve the copy compiled into the binary, because the scratch
// image has no files to mount.
func TestStaticFSFallsBackToEmbedded(t *testing.T) {
	t.Parallel()

	assets, err := staticFS(&config.Config{StaticDir: ""})
	require.NoError(t, err)

	got, err := fs.ReadFile(assets, "index.html")
	require.NoError(t, err, "index.html must be reachable at the root of the embedded tree")
	assert.Contains(t, string(got), "<html", "the embedded asset must be the real page, not an empty file")
}

// TestStaticFSSwitchIsNotInverted asserts the two sources actually differ, so
// an inverted STATIC_DIR check cannot pass both tests above by accident.
func TestStaticFSSwitchIsNotInverted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := "<h1>disk copy, not the embedded one</h1>"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.html"), []byte(marker), 0o600))

	fromDisk, err := staticFS(&config.Config{StaticDir: dir})
	require.NoError(t, err)
	diskBytes, err := fs.ReadFile(fromDisk, "index.html")
	require.NoError(t, err)

	embedded, err := staticFS(&config.Config{})
	require.NoError(t, err)
	embeddedBytes, err := fs.ReadFile(embedded, "index.html")
	require.NoError(t, err)

	assert.Equal(t, marker, string(diskBytes))
	assert.NotEqual(t, string(diskBytes), string(embeddedBytes))
}
