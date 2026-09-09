package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/config"
)

// env builds a getenv function over a map, so these tests never mutate the
// process environment and can run in parallel.
func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func complete() map[string]string {
	return map[string]string{
		"DATABASE_URL":   "postgres://inventory:pw@db:5432/inventory?sslmode=disable",
		"SESSION_SECRET": "a-generated-secret",
		"GEMINI_API_KEY": "gemini-key",
	}
}

// TestLoadReportsEveryMissingRequiredVariable checks the operator-facing
// failure. Reporting one variable per restart would make a fresh deployment a
// guessing game, so all of them must appear in a single message alongside the
// two commands that fix it.
func TestLoadReportsEveryMissingRequiredVariable(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{}))

	var missing *config.MissingError
	require.ErrorAs(t, err, &missing)
	assert.ElementsMatch(t,
		[]string{"DATABASE_URL", "SESSION_SECRET", "GEMINI_API_KEY"},
		missing.Names,
	)

	// Asserted whole, not by substring. The spec gives this message verbatim,
	// and it is the only thing an operator sees when a fresh deployment will
	// not start; a reword that keeps the tokens but drops "No configuration
	// found" or restructures the Run/Then lines would slip past a Contains
	// check while making the output worse.
	assert.Equal(t,
		"No configuration found (DATABASE_URL, SESSION_SECRET, GEMINI_API_KEY are unset).\n"+
			"Run:  docker compose run --rm setup\n"+
			"Then: docker compose up -d",
		err.Error(),
	)
}

// TestLoadTreatsBlankAsUnset covers the case that actually happens: a variable
// present in .env but left empty by the wizard. Accepting it would start the
// process with an empty DSN and fail later, far from the cause.
func TestLoadTreatsBlankAsUnset(t *testing.T) {
	t.Parallel()

	vars := complete()
	vars["SESSION_SECRET"] = "   "

	_, err := config.Load(env(vars))

	var missing *config.MissingError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, []string{"SESSION_SECRET"}, missing.Names)
	assert.Contains(t, err.Error(), "is unset", "a single missing variable reads in the singular")
}

// TestLoadAppliesDefaults pins the values a deployment gets when it sets only
// the required variables.
func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(complete()))
	require.NoError(t, err)

	assert.Equal(t, "prod", cfg.AppEnv)
	assert.Equal(t, "8000", cfg.HTTPPort)
	assert.Equal(t, "gemini-2.0-flash", cfg.GeminiModel)
	assert.Equal(t, "admin", cfg.AdminInitialUsername)
	assert.Empty(t, cfg.StaticDir, "empty STATIC_DIR must mean embedded assets")
	assert.Empty(t, cfg.GeminiImageModel, "the image model is optional and stays empty")
	assert.False(t, cfg.IsDev(), "a deployment that did not ask for dev must not get dev")
}

// TestIsDevOnlyForExactDevValue guards the flag that gates debug_reason
// disclosure (docs/specs/03-auth-and-multi-tenancy.md). Anything other than
// exactly "dev" must be treated as production.
func TestIsDevOnlyForExactDevValue(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"dev":         true,
		"prod":        false,
		"development": false,
		"DEV":         false,
		"":            false, // falls back to the prod default
	}

	for appEnv, wantDev := range tests {
		t.Run("APP_ENV="+appEnv, func(t *testing.T) {
			t.Parallel()

			vars := complete()
			vars["APP_ENV"] = appEnv

			cfg, err := config.Load(env(vars))
			require.NoError(t, err)
			assert.Equal(t, wantDev, cfg.IsDev())
		})
	}
}

// TestLoadTrimsValues covers the .env file that ends a line with a stray
// space; an untrimmed DSN fails to parse with an unhelpful message.
func TestLoadTrimsValues(t *testing.T) {
	t.Parallel()

	vars := complete()
	vars["DATABASE_URL"] = "  postgres://inventory:pw@db:5432/inventory  "

	cfg, err := config.Load(env(vars))
	require.NoError(t, err)
	assert.Equal(t, "postgres://inventory:pw@db:5432/inventory", cfg.DatabaseURL)
	assert.False(t, strings.HasSuffix(cfg.DatabaseURL, " "))
}

// TestLoadPanicsOnNilGetenv documents that a nil lookup is a programming error
// rather than an empty environment.
func TestLoadPanicsOnNilGetenv(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() { _, _ = config.Load(nil) })
}
