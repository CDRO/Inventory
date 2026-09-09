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

// optionalVars may end up empty. Everything else in .env.example must be
// given a value: an empty API key or DSN starts a process that fails later,
// far from the cause.
var optionalVars = map[string]bool{
	"STATIC_DIR":         true, // empty means "serve the embedded assets"
	"GEMINI_IMAGE_MODEL": true, // optional by design; empty disables the feature
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

		value, err := promptValue(reader, out, e)
		if err != nil {
			return err
		}
		e.value = value
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
	if key == "DATABASE_URL" {
		return validateDSN(value)
	}
	return ""
}

func validateDSN(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return "Not a valid URL: " + err.Error()
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return `Must start with "postgres://".`
	}
	if u.Host == "" {
		return "Missing host, e.g. postgres://user:pass@db:5432/inventory."
	}
	if strings.Trim(u.Path, "/") == "" {
		return "Missing database name, e.g. postgres://user:pass@db:5432/inventory."
	}
	return ""
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
// The whitespace before the '#' stays with the comment. It is what separates
// the value from the note when the line is written back out, and a .env line
// rendered as `APP_ENV=prod# note` would hand Compose the comment as part of
// the value.
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
// the file holds every secret the deployment has.
func writeEnv(path string, entries []entry) error {
	var b strings.Builder
	for _, e := range entries {
		if e.key == "" {
			b.WriteString(e.raw)
			b.WriteByte('\n')
			continue
		}
		b.WriteString(e.key)
		b.WriteByte('=')
		b.WriteString(e.value)
		b.WriteString(e.trailing)
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
