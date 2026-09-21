package config

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Filenames the wizard reads and writes, relative to its working directory.
const (
	ExampleFile = ".env.example"
	EnvFile     = ".env"
)

// generatedVars are never prompted for. A session secret typed by a human is a
// weak session secret, so it is taken from the system CSPRNG instead.
var generatedVars = map[string]bool{
	"SESSION_SECRET": true,
}

// derivedVars are never prompted for either. Their value is computed after
// every prompt has its final answer, from other entries, rather than typed —
// see deriveDatabaseURL.
var derivedVars = map[string]bool{
	"DATABASE_URL": true,
}

// optionalVars may end up empty. Everything else in .env.example must be
// given a value: an empty API key or DSN starts a process that fails later,
// far from the cause.
var optionalVars = map[string]bool{
	"STATIC_DIR":         true, // empty means "serve the embedded assets"
	"GEMINI_IMAGE_MODEL": true, // optional by design; empty disables the feature
	// SerpAPI backs shopping-list image suggestions
	// (07-shopping-list-reconciliation.md) and nothing in the startup path
	// needs it. Forcing a value here would make an operator who does not want
	// image search invent one, or loop on the prompt forever.
	"SERPAPI_API_KEY": true,
}

// sessionSecretBytes is 256 bits of entropy, base64url-encoded when written.
const sessionSecretBytes = 32

// Wizard implements `docker compose run --rm setup`: the interactive
// first-run flow that writes .env.
//
// It is a subcommand of the same Go binary rather than a shell script so that
// it behaves identically on Windows, macOS, and Linux — the prompts run inside
// the container, so no host runtime and no shell-portability problem exists
// (docs/specs/01-architecture-and-deployment.md).
type Wizard struct {
	In  io.Reader
	Out io.Writer
	// Dir is the bind-mounted project directory holding .env.example and
	// receiving .env. Empty means the process working directory.
	Dir string
}

// entry is one line of .env.example: either a passthrough (comment or blank)
// or a variable with its documented default and trailing comment.
type entry struct {
	raw      string // verbatim line, for passthrough
	key      string // empty for passthrough
	value    string
	trailing string // e.g. "  # only used in dev"
}

// Run executes the wizard. It returns an error rather than exiting so the
// caller owns the process exit code.
func (w *Wizard) Run() error {
	out := w.Out
	if out == nil {
		out = os.Stdout
	}
	in := w.In
	if in == nil {
		in = os.Stdin
	}

	examplePath := filepath.Join(w.Dir, ExampleFile)
	targetPath := filepath.Join(w.Dir, EnvFile)

	raw, err := os.ReadFile(examplePath)
	if err != nil {
		return fmt.Errorf("setup: read %s: %w", ExampleFile, err)
	}
	entries := parseExample(string(raw))

	reader := bufio.NewReader(in)

	existed := fileExists(targetPath)
	if existed {
		fmt.Fprintf(out, "%s already exists.\n", EnvFile)
		ok, err := confirm(reader, out, "Overwrite it?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "Left the existing file untouched.")
			return nil
		}
	}

	fmt.Fprintf(out, "\nWriting %s. Press Enter to accept the value in brackets.\n\n", EnvFile)

	for i := range entries {
		e := &entries[i]
		if e.key == "" {
			continue
		}
		if generatedVars[e.key] {
			secret, err := generateSecret()
			if err != nil {
				return err
			}
			e.value = secret
			fmt.Fprintf(out, "%-24s generated\n", e.key)
			continue
		}
		if derivedVars[e.key] {
			// Announced here, at the line's own position in the template —
			// the way a generated secret is — but not computed yet: a
			// POSTGRES_* prompt this entry depends on may still be ahead of
			// it in the template.
			fmt.Fprintf(out, "%-24s derived from POSTGRES_*\n", e.key)
			continue
		}

		value, err := promptValue(reader, out, e)
		if err != nil {
			return err
		}
		e.value = value
	}

	// Computed only now that every prompt has its final answer, regardless of
	// where DATABASE_URL's own line sits in the template, so a POSTGRES_*
	// prompt can never race the derivation that reads it.
	if err := deriveDatabaseURL(entries); err != nil {
		return err
	}

	if err := writeEnv(targetPath, entries); err != nil {
		return err
	}

	fmt.Fprintf(out, "\n%s written.\n", EnvFile)
	if existed {
		// Compose reads .env at file-parse time, so containers already running
		// hold the previous values until they are recreated.
		fmt.Fprintln(out, "Apply it with:")
		fmt.Fprintln(out, "  docker compose up -d --force-recreate")
	} else {
		fmt.Fprintln(out, "Start the stack with:")
		fmt.Fprintln(out, "  docker compose up -d")
	}
	return nil
}

// promptValue asks for one variable until the answer validates.
func promptValue(r *bufio.Reader, out io.Writer, e *entry) (string, error) {
	for {
		if e.value != "" {
			fmt.Fprintf(out, "%s [%s]: ", e.key, e.value)
		} else {
			fmt.Fprintf(out, "%s: ", e.key)
		}

		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("setup: read answer for %s: %w", e.key, err)
		}

		answer := strings.TrimSpace(line)
		if answer == "" {
			answer = e.value
		}

		if problem := validate(e.key, answer); problem != "" {
			fmt.Fprintf(out, "  %s\n", problem)
			continue
		}
		return answer, nil
	}
}

// validate returns an operator-facing problem description, or "" when the
// value is acceptable.
func validate(key, value string) string {
	if value == "" {
		if optionalVars[key] {
			return ""
		}
		return key + " cannot be empty."
	}
	if key == "POSTGRES_PASSWORD" && strings.ContainsRune(value, '$') {
		// Compose interpolates POSTGRES_PASSWORD from .env when it builds the
		// db service's environment (docker-compose.yml), but DATABASE_URL is
		// handed to the app via env_file, which Compose does not interpolate.
		// A '$' would then reach the two services differently, recreating the
		// exact password mismatch this spec exists to remove.
		return `POSTGRES_PASSWORD cannot contain "$": Compose interpolates this value for the db container but not inside DATABASE_URL, so the two would end up disagreeing.`
	}
	return ""
}

// deriveDatabaseURL computes DATABASE_URL from the final POSTGRES_USER,
// POSTGRES_PASSWORD and POSTGRES_DB answers and writes it into the matching
// entry. A template without a DATABASE_URL entry is left alone.
//
// Built with a URL builder rather than string concatenation, per
// docs/specs/30-setup-wizard-derived-config.md: user, password and database
// name are percent-encoded only where they need to be.
func deriveDatabaseURL(entries []entry) error {
	idx := -1
	for i := range entries {
		if entries[i].key == "DATABASE_URL" {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil
	}

	user, ok := entryValue(entries, "POSTGRES_USER")
	if !ok {
		return fmt.Errorf("setup: derive DATABASE_URL: %s has no POSTGRES_USER", ExampleFile)
	}
	password, ok := entryValue(entries, "POSTGRES_PASSWORD")
	if !ok {
		return fmt.Errorf("setup: derive DATABASE_URL: %s has no POSTGRES_PASSWORD", ExampleFile)
	}
	db, ok := entryValue(entries, "POSTGRES_DB")
	if !ok {
		return fmt.Errorf("setup: derive DATABASE_URL: %s has no POSTGRES_DB", ExampleFile)
	}

	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     "db:5432",
		Path:     "/" + db,
		RawQuery: "sslmode=disable",
	}
	entries[idx].value = u.String()
	return nil
}

// entryValue looks up a prompted or generated entry's final value by key.
func entryValue(entries []entry, key string) (string, bool) {
	for _, e := range entries {
		if e.key == key {
			return e.value, true
		}
	}
	return "", false
}

// confirm asks a yes/no question, defaulting to no.
func confirm(r *bufio.Reader, out io.Writer, question string) (bool, error) {
	fmt.Fprintf(out, "%s [y/N]: ", question)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("setup: read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// generateSecret returns 256 CSPRNG bits, base64url-encoded without padding.
func generateSecret() (string, error) {
	buf := make([]byte, sessionSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("setup: generate session secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// parseExample splits .env.example into ordered entries, keeping comments and
// blank lines so the generated .env stays as readable as the template.
func parseExample(text string) []entry {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	entries := make([]entry, 0, len(lines))

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		key, rest, found := strings.Cut(line, "=")
		if !found || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			entries = append(entries, entry{raw: line})
			continue
		}

		value, trailing := splitTrailingComment(rest)
		entries = append(entries, entry{
			key:      strings.TrimSpace(key),
			value:    strings.TrimSpace(value),
			trailing: trailing,
		})
	}
	return entries
}

// splitTrailingComment separates `value   # note` into its two halves. Only an
// unquoted '#' preceded by whitespace starts a comment, so a '#' inside a
// password is preserved.
//
// writeEnv renders trailing on its own line above the value, never after it:
// Compose reads a '#' after an *empty* value as part of the value, which is
// exactly the bug this parses around
// (docs/specs/30-setup-wizard-derived-config.md).
func splitTrailingComment(rest string) (value, trailing string) {
	for i := 1; i < len(rest); i++ {
		if rest[i] != '#' || (rest[i-1] != ' ' && rest[i-1] != '\t') {
			continue
		}
		start := i
		for start > 0 && (rest[start-1] == ' ' || rest[start-1] == '\t') {
			start--
		}
		return rest[:start], rest[start:]
	}
	return rest, ""
}

// writeEnv renders the entries and writes them with owner-only permissions —
// the file holds every secret the deployment has. A variable's note is
// written on its own line above the variable, never after the value on the
// same line: Compose reads a '#' after an empty value as part of the value
// (docs/specs/30-setup-wizard-derived-config.md).
func writeEnv(path string, entries []entry) error {
	var b strings.Builder
	for _, e := range entries {
		if e.key == "" {
			b.WriteString(e.raw)
			b.WriteByte('\n')
			continue
		}
		if note := strings.TrimSpace(e.trailing); note != "" {
			b.WriteString(note)
			b.WriteByte('\n')
		}
		b.WriteString(e.key)
		b.WriteByte('=')
		b.WriteString(e.value)
		b.WriteByte('\n')
	}

	// Trim the trailing blank line parseExample produces from the final newline.
	rendered := strings.TrimRight(b.String(), "\n") + "\n"

	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		return fmt.Errorf("setup: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
