package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/config"
)

const exampleFixture = `# --- Runtime ---
APP_ENV=prod                  # "dev" enables verbose error reasons
HTTP_PORT=8000
STATIC_DIR=

# --- Database ---
DATABASE_URL=postgres://inventory:changeme@db:5432/inventory?sslmode=disable

# --- Auth ---
SESSION_SECRET=

# --- Vision LLM ---
GEMINI_API_KEY=
GEMINI_IMAGE_MODEL=
`

// newProject writes the example file into a temp dir and returns its path.
func newProject(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.ExampleFile), []byte(exampleFixture), 0o600))
	return dir
}

// runWizard drives the wizard with scripted answers and returns its transcript.
func runWizard(t *testing.T, dir string, answers ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	w := &config.Wizard{
		In:  strings.NewReader(strings.Join(answers, "\n") + "\n"),
		Out: &out,
		Dir: dir,
	}
	err := w.Run()
	return out.String(), err
}

func readEnv(t *testing.T, dir string) map[string]string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, config.EnvFile))
	require.NoError(t, err)

	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, rest, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		value, _, _ := strings.Cut(rest, " #")
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values
}

// TestWizardGeneratesSessionSecret is the security-relevant behaviour: the
// session secret is never prompted for, because a value a human types is a
// value a human can guess.
func TestWizardGeneratesSessionSecret(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	// APP_ENV, HTTP_PORT, STATIC_DIR, DATABASE_URL accept defaults; then the
	// two keys. SESSION_SECRET is never asked for.
	transcript, err := runWizard(t, dir, "", "", "", "", "gemini-key", "")
	require.NoError(t, err)

	assert.NotContains(t, transcript, "SESSION_SECRET:", "the secret must not be prompted for")

	values := readEnv(t, dir)
	secret := values["SESSION_SECRET"]
	assert.GreaterOrEqual(t, len(secret), 43, "256 bits base64url-encodes to 43 characters")
	assert.NotContains(t, secret, "=", "the encoding is unpadded base64url")

	// A second run must not reproduce the first secret.
	other := newProject(t)
	_, err = runWizard(t, other, "", "", "", "", "gemini-key", "")
	require.NoError(t, err)
	assert.NotEqual(t, secret, readEnv(t, other)["SESSION_SECRET"], "each run generates fresh entropy")
}

// TestWizardAcceptsDefaultsOnEmptyInput covers the documented interaction:
// pressing Enter keeps the value shown in brackets.
func TestWizardAcceptsDefaultsOnEmptyInput(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	_, err := runWizard(t, dir, "", "", "", "", "gemini-key", "")
	require.NoError(t, err)

	values := readEnv(t, dir)
	assert.Equal(t, "prod", values["APP_ENV"])
	assert.Equal(t, "8000", values["HTTP_PORT"])
	assert.Equal(t, "postgres://inventory:changeme@db:5432/inventory?sslmode=disable", values["DATABASE_URL"])
	assert.Empty(t, values["STATIC_DIR"], "an optional variable may stay empty")
	assert.Empty(t, values["GEMINI_IMAGE_MODEL"], "the image model is optional by design")
}

// TestWizardRejectsEmptyAPIKeyAndReprompts covers the validation the spec asks
// for. A blank key written to .env starts a process that fails on its first
// vision call, far from the cause.
func TestWizardRejectsEmptyAPIKeyAndReprompts(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	transcript, err := runWizard(t, dir, "", "", "", "", "", "real-key", "")
	require.NoError(t, err)

	assert.Contains(t, transcript, "GEMINI_API_KEY cannot be empty.")
	assert.Equal(t, "real-key", readEnv(t, dir)["GEMINI_API_KEY"])
}

// TestWizardRejectsMalformedDSNAndReprompts is the other validation named in
// the spec.
func TestWizardRejectsMalformedDSNAndReprompts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bad  string
		want string
	}{
		{name: "wrong scheme", bad: "mysql://user:pw@db:3306/inventory", want: `Must start with "postgres://".`},
		{name: "no host", bad: "postgres:///inventory", want: "Missing host"},
		{name: "no database", bad: "postgres://user:pw@db:5432", want: "Missing database name"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := newProject(t)
			good := "postgres://inventory:pw@db:5432/inventory"

			transcript, err := runWizard(t, dir, "", "", "", tc.bad, good, "gemini-key", "")
			require.NoError(t, err)

			assert.Contains(t, transcript, tc.want)
			assert.Equal(t, good, readEnv(t, dir)["DATABASE_URL"])
		})
	}
}

// TestWizardRefusesToOverwriteWithoutConfirmation protects a configured
// deployment from a stray `docker compose run --rm setup`.
func TestWizardRefusesToOverwriteWithoutConfirmation(t *testing.T) {
	t.Parallel()

	dir := newProject(t)
	existing := "APP_ENV=prod\nSESSION_SECRET=do-not-lose-me\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.EnvFile), []byte(existing), 0o600))

	transcript, err := runWizard(t, dir, "n")
	require.NoError(t, err)

	assert.Contains(t, transcript, "already exists")
	raw, err := os.ReadFile(filepath.Join(dir, config.EnvFile))
	require.NoError(t, err)
	assert.Equal(t, existing, string(raw), "declining must leave the file byte-identical")
}

// TestWizardOverwritesOnConfirmationAndExplainsRecreate covers the other half:
// Compose reads .env at parse time, so a running stack keeps the old values
// until it is recreated. Not saying so leaves the operator thinking the new
// key did not take.
func TestWizardOverwritesOnConfirmationAndExplainsRecreate(t *testing.T) {
	t.Parallel()

	dir := newProject(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.EnvFile), []byte("APP_ENV=prod\n"), 0o600))

	transcript, err := runWizard(t, dir, "y", "", "", "", "", "gemini-key", "")
	require.NoError(t, err)

	assert.Contains(t, transcript, "docker compose up -d --force-recreate")
	assert.Equal(t, "gemini-key", readEnv(t, dir)["GEMINI_API_KEY"])
}

// TestWizardPreservesCommentsAndOrder keeps the generated file as readable as
// the template an operator is about to edit by hand.
func TestWizardPreservesCommentsAndOrder(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	_, err := runWizard(t, dir, "", "", "", "", "gemini-key", "")
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, config.EnvFile))
	require.NoError(t, err)
	rendered := string(raw)

	assert.Contains(t, rendered, "# --- Runtime ---")
	assert.Contains(t, rendered, "# --- Database ---")
	assert.Less(t,
		strings.Index(rendered, "APP_ENV="),
		strings.Index(rendered, "DATABASE_URL="),
		"variables keep the order of the template",
	)
	assert.Contains(t, rendered, `# "dev" enables verbose error reasons`, "trailing comments survive")
}

// TestWizardWritesSecretsOwnerOnly checks the permissions on the one file that
// holds every secret the deployment has.
func TestWizardWritesSecretsOwnerOnly(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}

	dir := newProject(t)
	_, err := runWizard(t, dir, "", "", "", "", "gemini-key", "")
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(dir, config.EnvFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestWizardFailsWithoutExample reports the packaging error rather than
// writing an empty .env.
func TestWizardFailsWithoutExample(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, err := runWizard(t, dir, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), config.ExampleFile)
	assert.NoFileExists(t, filepath.Join(dir, config.EnvFile))
}

// TestWizardFirstRunPrintsStartHint covers the other closing message. On a
// fresh install the next step is `up -d`, not `--force-recreate`; telling a
// first-time operator to recreate containers that do not exist reads as an
// error.
func TestWizardFirstRunPrintsStartHint(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	transcript, err := runWizard(t, dir, "", "", "", "", "gemini-key", "")
	require.NoError(t, err)

	assert.Contains(t, transcript, "docker compose up -d")
	assert.NotContains(t, transcript, "--force-recreate", "nothing is running yet on a first run")
}

// exampleKeys returns the variables of a .env.example in file order.
func exampleKeys(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var keys []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if key, _, found := strings.Cut(line, "="); found {
			keys = append(keys, strings.TrimSpace(key))
		}
	}
	return keys
}

// TestWizardAgainstRealExample drives the wizard with the repository's actual
// .env.example rather than a reduced fixture.
//
// The wizard echoes whatever the template contains, so every other test in
// this file would stay green if the real file drifted — gained a variable the
// spec forbids, or lost a required one. This is the only test that looks at
// the shipped file.
func TestWizardAgainstRealExample(t *testing.T) {
	t.Parallel()

	repoExample := filepath.Join("..", "..", config.ExampleFile)
	keys := exampleKeys(t, repoExample)
	require.NotEmpty(t, keys, "the repository .env.example must define variables")

	dir := t.TempDir()
	raw, err := os.ReadFile(repoExample)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.ExampleFile), raw, 0o600))

	// SESSION_SECRET is generated, never prompted. Everything else takes its
	// documented default except the one variable that ships empty and is
	// required — supplying a value there is the operator's job.
	var answers []string
	for _, key := range keys {
		switch key {
		case "SESSION_SECRET":
			continue
		case "GEMINI_API_KEY":
			answers = append(answers, "a-real-gemini-key")
		default:
			answers = append(answers, "")
		}
	}

	transcript, err := runWizard(t, dir, answers...)
	require.NoError(t, err, "the shipped .env.example must be completable with defaults plus the API key.\n%s", transcript)

	values := readEnv(t, dir)

	for _, key := range keys {
		assert.Contains(t, values, key, "%s from .env.example must reach .env", key)
	}
	assert.GreaterOrEqual(t, len(values["SESSION_SECRET"]), 43, "the secret is generated, not copied from the template")
	assert.Equal(t, "a-real-gemini-key", values["GEMINI_API_KEY"])
	assert.Equal(t, "prod", values["APP_ENV"], "the shipped default must not be dev")

	// The frontend calls relative /api paths because Traefik serves UI and API
	// from one origin; a base-URL variable here would mean that stopped being
	// true (docs/specs/01-architecture-and-deployment.md).
	for _, key := range keys {
		assert.NotContains(t, key, "API_URL", "the spec forbids a frontend API-URL variable")
		assert.NotContains(t, key, "FRONTEND", "the spec forbids a frontend API-URL variable")
	}

	// SerpAPI backs a spec-07 feature; a deployment that does not want image
	// search must be able to leave it empty rather than invent a key.
	assert.Empty(t, values["SERPAPI_API_KEY"], "SERPAPI_API_KEY must be completable as empty")
	assert.Empty(t, values["GEMINI_IMAGE_MODEL"], "the image model is optional by design")
}
