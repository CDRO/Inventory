// Package config loads the application's runtime configuration from the
// process environment and reports, in one place, when that configuration is
// unusable.
//
// Configuration that must change without a redeploy does not live here: the
// effective Gemini model is resolved against the settings table at request
// time (see package vision and docs/specs/01-architecture-and-deployment.md),
// because a running container cannot pick up a rewritten .env without being
// recreated.
package config

import (
	"fmt"
	"strings"
)

// ExitConfig is the process exit code used for an unusable configuration. It
// is distinct from a generic failure so that an operator reading
// `docker compose logs app` can tell "misconfigured" from "crashed", and it is
// deliberately non-retryable: the container must stay down rather than restart
// into the same error and scroll the actionable message out of the log.
const ExitConfig = 78 // EX_CONFIG, sysexits.h

// requiredVars are the variables without which the process cannot serve a
// single request. They are reported together rather than one per restart.
var requiredVars = []string{"DATABASE_URL", "SESSION_SECRET", "GEMINI_API_KEY"}

// Config is the fully-resolved environment configuration.
type Config struct {
	AppEnv    string
	HTTPPort  string
	StaticDir string // empty = serve the assets embedded in the binary

	DatabaseURL string

	SessionSecret        string
	AdminInitialUsername string
	AdminInitialPassword string

	GeminiAPIKey string
	// GeminiModel is the environment's model id. It is only the fallback: a
	// gemini_model row in the settings table outranks it.
	GeminiModel string
	// GeminiImageModel is optional by design. Empty disables the
	// background-removal offer in docs/specs/09-consumption-logging.md and is
	// not a degraded state.
	GeminiImageModel string

	SerpAPIKey string
}

// IsDev reports whether verbose error reasons are permitted. Every caller that
// might disclose an internal reason must consult this rather than testing
// APP_ENV itself, so the check exists in one place.
func (c *Config) IsDev() bool { return c.AppEnv == "dev" }

// MissingError names the required variables that were unset or empty. Its
// message is the operator-facing remediation text printed at startup.
type MissingError struct {
	Names []string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf(
		"No configuration found (%s %s unset).\nRun:  docker compose run --rm setup\nThen: docker compose up -d",
		strings.Join(e.Names, ", "),
		map[bool]string{true: "is", false: "are"}[len(e.Names) == 1],
	)
}

// Load reads the configuration using getenv, which is a parameter rather than
// a direct os.Getenv call so the loader is testable without mutating the
// process environment. A nil getenv is a programming error and panics.
//
// It returns *MissingError when a required variable is unset or empty; the
// caller is expected to print that message and exit with ExitConfig.
func Load(getenv func(string) string) (*Config, error) {
	if getenv == nil {
		panic("config.Load: getenv must not be nil")
	}

	get := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}

	var missing []string
	for _, name := range requiredVars {
		if strings.TrimSpace(getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, &MissingError{Names: missing}
	}

	return &Config{
		AppEnv:    get("APP_ENV", "prod"),
		HTTPPort:  get("HTTP_PORT", "8000"),
		StaticDir: strings.TrimSpace(getenv("STATIC_DIR")),

		DatabaseURL: strings.TrimSpace(getenv("DATABASE_URL")),

		SessionSecret:        strings.TrimSpace(getenv("SESSION_SECRET")),
		AdminInitialUsername: get("ADMIN_INITIAL_USERNAME", "admin"),
		AdminInitialPassword: strings.TrimSpace(getenv("ADMIN_INITIAL_PASSWORD")),

		GeminiAPIKey:     strings.TrimSpace(getenv("GEMINI_API_KEY")),
		GeminiModel:      get("GEMINI_MODEL", "gemini-2.0-flash"),
		GeminiImageModel: strings.TrimSpace(getenv("GEMINI_IMAGE_MODEL")),

		SerpAPIKey: strings.TrimSpace(getenv("SERPAPI_API_KEY")),
	}, nil
}
