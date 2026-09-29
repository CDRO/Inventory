package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workflowBlock returns the lines of a top-level YAML key's block (`on:`,
// `jobs:`, ...) from a GitHub Actions workflow file, comments dropped — the
// same text-scan approach compose_test.go's composeService uses for a
// compose file's service map, adapted to a workflow file's shape: a
// top-level key sits at column 0, and its block runs until the next line
// that dedents back to column 0 and is not itself a comment.
//
// Text rather than a YAML parse for the same reason composeService gives:
// gopkg.in/yaml.v3 is currently only an indirect dependency, and promoting
// it to read a handful of trigger lines is a worse trade than this reader.
func workflowBlock(t *testing.T, file, key string) string {
	t.Helper()

	// Normalized first: this repository is checked out on Windows with
	// core.autocrlf=true, so a line-exact match on "key:" would otherwise
	// silently find nothing on one of the two checkout styles.
	normalized := strings.ReplaceAll(repoFile(t, file), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	start := -1
	for i, line := range lines {
		if line == key+":" {
			start = i + 1
			break
		}
	}
	require.NotEqual(t, -1, start, "%s must declare a top-level %q key", file, key)

	var block []string
	for _, line := range lines[start:] {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			// Dedented to column 0 and not a comment: the next top-level key.
			break
		}
		if code, _, found := strings.Cut(line, " #"); found {
			line = strings.TrimRight(code, " ")
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		block = append(block, line)
	}
	return strings.Join(block, "\n")
}

// TestE2EWorkflowTriggerShape is issue #359 item 3 for .github/workflows/
// e2e.yml: nothing in `go test ./...` covered this file's own trigger set
// before now, so a typo'd or narrowed trigger could silently stop the
// deployment gate from ever running again — a failure mode no green suite
// distinguishes from "nothing needed to run".
//
// The header's own reasoning (restated there at length) is that this suite
// deliberately runs on `push` to `main`, `workflow_dispatch` and
// `workflow_call` (release.yml's gate, decision H17) and NEVER on
// `pull_request` — E2E is the deployment gate, not a PR merge gate. The
// negative assertion is the half that fails silently: a `pull_request`
// trigger added here by mistake would not break a single other test and
// would only ever be noticed by CI minutes disappearing into a PR queue.
func TestE2EWorkflowTriggerShape(t *testing.T) {
	t.Parallel()

	block := workflowBlock(t, ".github/workflows/e2e.yml", "on")

	assert.Contains(t, block, "  push:", "e2e.yml must trigger on push")
	assert.Contains(t, block, "    branches: [main]",
		"e2e.yml's push trigger must be scoped to main, not every branch")
	assert.Contains(t, block, "  workflow_dispatch:",
		"e2e.yml must support an on-demand dispatch for a pre-deploy or debugging run")
	assert.Contains(t, block, "  workflow_call:",
		"e2e.yml must be callable: release.yml (H17) reuses it as the tag-commit deployment gate")
	assert.NotContains(t, block, "pull_request",
		"e2e.yml must never trigger on pull_request — it is the deployment gate, not a PR merge gate (see this workflow's own header)")
}

// TestRestoreWorkflowTriggerShape is issue #359 item 3 for
// .github/workflows/restore.yml: decision D8 (docs/plans/2026-09-harness-
// optimization.md §9) narrowed this job from "every push to main" to a
// path-filtered trigger plus a tag push and a weekly schedule, specifically
// so that only files that can actually break the backup/restore round trip
// re-run it. A path filter that silently drifted from that file list — a
// typo, a renamed directory nobody updated here — would mean the round trip
// quietly stops running for changes it exists to catch, and a green suite
// cannot tell that apart from "nothing needed to run" either.
func TestRestoreWorkflowTriggerShape(t *testing.T) {
	t.Parallel()

	block := workflowBlock(t, ".github/workflows/restore.yml", "on")

	assert.Contains(t, block, "  push:", "restore.yml must trigger on push")
	assert.Contains(t, block, "    branches: [main]",
		"restore.yml's push trigger must include main")
	assert.Contains(t, block, `    tags: ["v*"]`,
		"restore.yml must run unconditionally on a version tag push — the deployment gate spec 18 calls the only rollback")
	assert.Contains(t, block, "    paths:", "restore.yml's push trigger must be path-filtered (D8)")

	// D8's file list, pulled from this workflow's own header comment rather
	// than re-derived: the set of files that can actually break the round
	// trip. Enumerated individually so a missing or renamed entry names
	// itself instead of failing one opaque Contains on the whole block.
	for _, path := range []string{
		"scripts/backup",
		"docker-compose*.yml",
		"migrations/**",
		"e2e/restore/**",
		"e2e/fixtures/**",
		"Dockerfile",
	} {
		assert.Contains(t, block, "      - "+path,
			"restore.yml's push path filter is missing %q (D8)", path)
	}

	assert.Contains(t, block, `  schedule:`,
		"restore.yml must run on a schedule to catch postgres:16-alpine image drift no path filter can see")
	assert.Contains(t, block, `    - cron: "17 3 * * 1"`,
		"restore.yml's schedule trigger has drifted from its documented weekly cron")
	assert.Contains(t, block, "  workflow_dispatch:",
		"restore.yml must support an on-demand dispatch, the same escape hatch every other workflow here has")
}
