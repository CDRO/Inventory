package config

import "fmt"

// RenderEnv produces a complete .env file, in exactly .env.example's shape
// (docs/specs/01-architecture-and-deployment.md), from the currently running
// configuration — for the admin download at GET /api/admin/settings/env-file.
//
// effectiveGeminiModel is written as GEMINI_MODEL instead of c.GeminiModel:
// the whole point of the download is "regenerated with the corrected model
// line" when an operator has fixed a dead model id from the settings-table
// override rather than the environment, and a file that still named the
// stale env value would not actually fix anything on redeploy.
//
// Every other value, including secrets (SESSION_SECRET, the API keys,
// POSTGRES_PASSWORD), is written verbatim. That is deliberate, not an
// oversight: this only ever reaches an already-authenticated admin
// (RequireAdmin re-checked on every request), and a .env with placeholder
// secrets would not be "regenerated" in the sense the spec means — an
// operator redeploying from it needs the stack to actually come back up.
func RenderEnv(c *Config, effectiveGeminiModel string) string {
	return fmt.Sprintf(`# --- Runtime ---
APP_ENV=%s
HTTP_PORT=%s
STATIC_DIR=%s

# --- Database ---
POSTGRES_USER=%s
POSTGRES_PASSWORD=%s
POSTGRES_DB=%s
DATABASE_URL=%s

# --- Auth ---
SESSION_SECRET=%s
ADMIN_INITIAL_USERNAME=%s
ADMIN_INITIAL_PASSWORD=%s

# --- Vision LLM ---
GEMINI_API_KEY=%s
GEMINI_MODEL=%s
GEMINI_IMAGE_MODEL=%s

# --- Shopping-list image suggestions ---
SERPAPI_API_KEY=%s
`,
		c.AppEnv, c.HTTPPort, c.StaticDir,
		c.PostgresUser, c.PostgresPassword, c.PostgresDB, c.DatabaseURL,
		c.SessionSecret, c.AdminInitialUsername, c.AdminInitialPassword,
		c.GeminiAPIKey, effectiveGeminiModel, c.GeminiImageModel,
		c.SerpAPIKey)
}
