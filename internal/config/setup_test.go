package config_test

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/config"
)

const exampleFixture = `# --- Runtime ---
APP_ENV=prod
HTTP_PORT=8000
STATIC_DIR=

# --- Database ---
POSTGRES_USER=inventory
POSTGRES_PASSWORD=changeme
POSTGRES_DB=inventory
DATABASE_URL=postgres://inventory:changeme@db:5432/inventory?sslmode=disable

# --- Auth ---
SESSION_SECRET=

# --- Vision LLM ---
GEMINI_API_KEY=
GEMINI_IMAGE_MODEL=
`

// fixtureDefaults answers every prompt in exampleFixture with its default
// (Enter), except GEMINI_API_KEY, which is required. Order: APP_ENV,
// HTTP_PORT, STATIC_DIR, POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB,
// GEMINI_API_KEY, GEMINI_IMAGE_MODEL. DATABASE_URL and SESSION_SECRET are
// never prompted for.
func fixtureDefaults(geminiKey string) []string {
	return []string{"", "", "", "", "", "", geminiKey, ""}
}

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
		values[strings.TrimSpace(key)] = strings.TrimSpace(rest)
	}
	return values
}

// TestWizardGeneratesSessionSecret is the security-relevant behaviour: the
// session secret is never prompted for, because a value a human types is a
// value a human can guess.
func TestWizardGeneratesSessionSecret(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	transcript, err := runWizard(t, dir, fixtureDefaults("gemini-key")...)
	require.NoError(t, err)

	assert.NotContains(t, transcript, "SESSION_SECRET:", "the secret must not be prompted for")

	values := readEnv(t, dir)
	secret := values["SESSION_SECRET"]
	assert.GreaterOrEqual(t, len(secret), 43, "256 bits base64url-encodes to 43 characters")
	assert.NotContains(t, secret, "=", "the encoding is unpadded base64url")

	// A second run must not reproduce the first secret.
	other := newProject(t)
	_, err = runWizard(t, other, fixtureDefaults("gemini-key")...)
	require.NoError(t, err)
	assert.NotEqual(t, secret, readEnv(t, other)["SESSION_SECRET"], "each run generates fresh entropy")
}

// TestWizardAcceptsDefaultsOnEmptyInput covers the documented interaction:
// pressing Enter keeps the value shown in brackets.
func TestWizardAcceptsDefaultsOnEmptyInput(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	_, err := runWizard(t, dir, fixtureDefaults("gemini-key")...)
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

	transcript, err := runWizard(t, dir, "", "", "", "", "", "", "", "real-key", "")
	require.NoError(t, err)

	assert.Contains(t, transcript, "GEMINI_API_KEY cannot be empty.")
	assert.Equal(t, "real-key", readEnv(t, dir)["GEMINI_API_KEY"])
}

// TestWizardNeverPromptsForDatabaseURL is the defect spec 30 exists to fix:
// DATABASE_URL used to be a second, independent source of truth for the same
// three values POSTGRES_* already answer.
func TestWizardNeverPromptsForDatabaseURL(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	transcript, err := runWizard(t, dir, fixtureDefaults("gemini-key")...)
	require.NoError(t, err)

	assert.NotContains(t, transcript, "DATABASE_URL:", "DATABASE_URL must not be prompted for")
	assert.Contains(t, transcript, "DATABASE_URL", "the derivation is still announced, the way a generated secret is")
}

// TestWizardDerivesDatabaseURLFromPostgresAnswers asserts the exact string:
// non-default POSTGRES_* answers must change DATABASE_URL to match, and
// nothing from the template's own DATABASE_URL line may survive into it.
func TestWizardDerivesDatabaseURLFromPostgresAnswers(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	// APP_ENV, HTTP_PORT, STATIC_DIR default; POSTGRES_USER, PASSWORD, DB set;
	// GEMINI_API_KEY required; GEMINI_IMAGE_MODEL default.
	transcript, err := runWizard(t, dir, "", "", "", "custom_user", "custom_pw", "custom_db", "gemini-key", "")
	require.NoError(t, err, transcript)

	assert.Equal(t,
		"postgres://custom_user:custom_pw@db:5432/custom_db?sslmode=disable",
		readEnv(t, dir)["DATABASE_URL"],
	)

	// A second run with different answers must not reuse the first URL, and
	// must not reuse the template's own default either.
	other := newProject(t)
	_, err = runWizard(t, other, "", "", "", "second_user", "second_pw", "second_db", "gemini-key", "")
	require.NoError(t, err)

	first := readEnv(t, dir)["DATABASE_URL"]
	second := readEnv(t, other)["DATABASE_URL"]
	assert.NotEqual(t, first, second, "two different sets of POSTGRES_* answers must write two different URLs")
	assert.NotEqual(t,
		"postgres://inventory:changeme@db:5432/inventory?sslmode=disable",
		second,
		"the template's own DATABASE_URL default must not survive",
	)
}

// TestWizardDatabaseURLRoundTripsSpecialCharacters covers the round-trip
// acceptance criterion: a password built with a URL builder, not string
// concatenation, must parse back to exactly what was entered even when it
// contains characters that are meaningful in a URL.
func TestWizardDatabaseURLRoundTripsSpecialCharacters(t *testing.T) {
	t.Parallel()

	const password = `p@ss:w/o?r#d%&=+ ` + "é" // trailing non-ASCII letter (é)

	dir := newProject(t)
	_, err := runWizard(t, dir, "", "", "", "custom_user", password, "custom_db", "gemini-key", "")
	require.NoError(t, err)

	dsn := readEnv(t, dir)["DATABASE_URL"]
	u, err := url.Parse(dsn)
	require.NoError(t, err, "the derived DATABASE_URL must itself be a valid URL")

	gotPassword, ok := u.User.Password()
	require.True(t, ok)
	assert.Equal(t, "custom_user", u.User.Username())
	assert.Equal(t, password, gotPassword)
	assert.Equal(t, "/custom_db", u.Path)
}

// TestWizardRejectsDollarInPostgresPassword covers the explicit decision the
// spec asks for: Compose interpolates POSTGRES_PASSWORD when it builds the
// db container's environment but does not interpolate DATABASE_URL (handed
// to the app via env_file), so a '$' would reach the two services
// differently and recreate the very mismatch this spec removes.
func TestWizardRejectsDollarInPostgresPassword(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	transcript, err := runWizard(t, dir, "", "", "", "", "sec$ret", "good-password", "", "gemini-key", "")
	require.NoError(t, err)

	assert.Contains(t, transcript, `POSTGRES_PASSWORD cannot contain "$"`)
	assert.Equal(t, "good-password", readEnv(t, dir)["POSTGRES_PASSWORD"])
	assert.Contains(t, readEnv(t, dir)["DATABASE_URL"], "good-password")
}

// TestWizardRejectsWhitespaceHashInPostgresPassword covers the same mismatch
// class as the '$' rejection, but via the other route: Compose reads a '#'
// preceded by whitespace as a comment when it reads .env to build the db
// container's environment (the exact rule docs/specs/30 names as the second
// defect), silently truncating POSTGRES_PASSWORD there. DATABASE_URL keeps
// the password intact because net/url percent-encodes '#', so an unrejected
// "pass word#tail" would leave db and the app with two different passwords.
func TestWizardRejectsWhitespaceHashInPostgresPassword(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	transcript, err := runWizard(t, dir, "", "", "", "", "pass word #tail", "good-password", "", "gemini-key", "")
	require.NoError(t, err)

	assert.Contains(t, transcript, `POSTGRES_PASSWORD cannot contain whitespace followed by "#"`)
	assert.Equal(t, "good-password", readEnv(t, dir)["POSTGRES_PASSWORD"])
	assert.Contains(t, readEnv(t, dir)["DATABASE_URL"], "good-password")
}

// TestWizardAcceptsHashWithoutPrecedingWhitespaceInPostgresPassword checks the
// validation is not overbroad: a '#' with no whitespace before it is not a
// comment to Compose either (that is the first defect this spec fixes, for an
// empty value), so it must remain a legal password character.
func TestWizardAcceptsHashWithoutPrecedingWhitespaceInPostgresPassword(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	const password = "pass#word"
	_, err := runWizard(t, dir, "", "", "", "", password, "", "gemini-key", "")
	require.NoError(t, err)

	assert.Equal(t, password, readEnv(t, dir)["POSTGRES_PASSWORD"])
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

	answers := append([]string{"y"}, fixtureDefaults("gemini-key")...)
	transcript, err := runWizard(t, dir, answers...)
	require.NoError(t, err)

	assert.Contains(t, transcript, "docker compose up -d --force-recreate")
	assert.Equal(t, "gemini-key", readEnv(t, dir)["GEMINI_API_KEY"])
}

// TestWizardPreservesCommentsAndOrder keeps the generated file as readable as
// the template an operator is about to edit by hand.
func TestWizardPreservesCommentsAndOrder(t *testing.T) {
	t.Parallel()

	dir := newProject(t)

	_, err := runWizard(t, dir, fixtureDefaults("gemini-key")...)
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
}

// TestWizardMovesInlineCommentsAboveTheValue is the other defect spec 30
// fixes: Compose reads a '#' after an *empty* value as part of the value, so
// no generated line may carry a trailing comment — the note moves to its own
// line above the variable instead.
func TestWizardMovesInlineCommentsAboveTheValue(t *testing.T) {
	t.Parallel()

	// Reproduces the pre-fix .env.example shape: notes after the value, on
	// the same line.
	const fixtureWithInlineNotes = `APP_ENV=prod                  # "dev" enables verbose error reasons
POSTGRES_USER=inventory
POSTGRES_PASSWORD=changeme
POSTGRES_DB=inventory
DATABASE_URL=postgres://inventory:changeme@db:5432/inventory?sslmode=disable
SESSION_SECRET=
GEMINI_API_KEY=                # vision key
`
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.ExampleFile), []byte(fixtureWithInlineNotes), 0o600))

	// APP_ENV, POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB default;
	// GEMINI_API_KEY required.
	_, err := runWizard(t, dir, "", "", "", "", "real-key")
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, config.EnvFile))
	require.NoError(t, err)
	rendered := string(raw)

	// [^\n]*[ \t]# rather than .*\s# — Go's \s matches '\n', which would let
	// the pattern span into the next line's leading comment and false-positive
	// on exactly the shape this fix produces (a value line directly followed
	// by the next variable's note).
	noInlineComment := regexp.MustCompile(`(?m)^[A-Z_]+=[^\n]*[ \t]#`)
	assert.False(t, noInlineComment.MatchString(rendered), "no value line may carry a trailing comment:\n%s", rendered)

	assert.Regexp(t, `(?m)^# "dev" enables verbose error reasons\nAPP_ENV=prod$`, rendered, "the note moves above the value, not after it")
	assert.Regexp(t, `(?m)^# vision key\nGEMINI_API_KEY=real-key$`, rendered, "the note moves above the value, not after it")
}

// TestWizardWritesSecretsOwnerOnly checks the permissions on the one file that
// holds every secret the deployment has.
func TestWizardWritesSecretsOwnerOnly(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}

	dir := newProject(t)
	_, err := runWizard(t, dir, fixtureDefaults("gemini-key")...)
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

	transcript, err := runWizard(t, dir, fixtureDefaults("gemini-key")...)
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

	// SESSION_SECRET is generated and DATABASE_URL is derived — neither is
	// ever prompted for. Everything else takes its documented default except
	// the one variable that ships empty and is required — supplying a value
	// there is the operator's job.
	var answers []string
	for _, key := range keys {
		switch key {
		case "SESSION_SECRET", "DATABASE_URL":
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
	assert.Equal(t,
		"postgres://inventory:changeme@db:5432/inventory?sslmode=disable",
		values["DATABASE_URL"],
		"defaults for POSTGRES_* must derive the shipped default DATABASE_URL",
	)

	// The frontend calls relative /api paths because Traefik serves UI and API
	// from one origin; a base-URL variable here would mean that stopped being
	// true (docs/specs/01-architecture-and-deployment.md).
	for _, key := range keys {
		assert.NotContains(t, key, "API_URL", "the spec forbids a frontend API-URL variable")
		assert.NotContains(t, key, "FRONTEND", "the spec forbids a frontend API-URL variable")
	}

	// COMPOSE_FILE (or any other deployment selector) is out of scope: the
	// wizard prompts for every key in .env.example (docs/specs/01, Synology
	// section).
	assert.NotContains(t, keys, "COMPOSE_FILE", "no deployment-selector variable belongs in .env.example")

	// SerpAPI backs a spec-07 feature; a deployment that does not want image
	// search must be able to leave it empty rather than invent a key.
	assert.Empty(t, values["SERPAPI_API_KEY"], "SERPAPI_API_KEY must be completable as empty")
	assert.Empty(t, values["GEMINI_IMAGE_MODEL"], "the image model is optional by design")

	// No generated line may carry a trailing comment (docs/specs/30): Compose
	// reads a '#' after an empty value as part of the value.
	noInlineComment := regexp.MustCompile(`(?m)^[A-Z_]+=[^\n]*[ \t]#`)
	rawEnv, err := os.ReadFile(filepath.Join(dir, config.EnvFile))
	require.NoError(t, err)
	assert.False(t, noInlineComment.MatchString(string(rawEnv)), "no value line in the shipped .env.example may carry a trailing comment:\n%s", rawEnv)
}
