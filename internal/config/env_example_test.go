package config_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/config"
)

// TestEnvExampleParity is the docs reviewer's most frequent finding (H6,
// docs/plans/2026-09-harness-optimization.md) turned into a test: an operator
// can only learn a variable exists by reading .env.example, so every key
// Load reads must be declared there, and every key declared there must
// actually be read — an entry nobody reads is a lie about what configures
// the deployment.
//
// The loader's side is captured by recording every key a getenv closure is
// asked for, rather than hand-maintaining a second list next to Load: a
// hand-maintained list can drift the moment a key is added to one and not
// the other, which is exactly the bug class this test exists to catch.
func TestEnvExampleParity(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", config.ExampleFile))
	require.NoError(t, err, "%s must exist at the repository root", config.ExampleFile)

	exampleKeys := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		exampleKeys[strings.TrimSpace(key)] = true
	}

	loaderKeys := map[string]bool{}
	// Every key answers with the same non-empty value: Load does no format
	// validation, only presence, so this is enough to reach a successful load
	// and record every key asked for along the way.
	_, err = config.Load(func(key string) string {
		loaderKeys[key] = true
		return "x"
	})
	require.NoError(t, err, "a value for every key must be enough to load")

	assert.ElementsMatch(t, slices.Collect(maps.Keys(exampleKeys)), slices.Collect(maps.Keys(loaderKeys)),
		"every key .env.example declares must be read by the loader, and every key "+
			"the loader reads must be declared in .env.example")
}
