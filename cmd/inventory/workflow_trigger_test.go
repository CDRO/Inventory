package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workflowFile reads a workflow file from the repository root, or skips the
// test when it is not there at all.
//
// The skip is for exactly one caller, and it is the same one
// scripts/release_workflow_test.go's workflowFiles documents: the Dockerfile's
// builder stage runs `go test ./...` against the build context, and
// `.dockerignore` excludes `.github` from it — workflow files have no business
// in a production image. Without the skip these assertions do not merely fail
// to run there, they fail the image build itself, which takes the E2E gate
// (`docker compose -f docker-compose.e2e.yml build`) and the release build down
// with it. Everywhere the suite is actually a gate — a local
// `docker compose run --rm app go test ./...`, a reviewer's run, and CI's
// `test` job, all of which merge docker-compose.override.yml — the directory is
// bind-mounted and every assertion below runs.
func workflowFile(t *testing.T, name string) string {
	t.Helper()

	if _, err := os.Stat(filepath.Join("..", "..", name)); os.IsNotExist(err) {
		t.Skipf("%s is not present (the image build's context excludes .github) - nothing to check here", name)
	}
	return repoFile(t, name)
}

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
	normalized := strings.ReplaceAll(workflowFile(t, file), "\r\n", "\n")
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

// workflowStep returns one job step's own lines (its `run:`/`with:` body,
// comments dropped) from a GitHub Actions workflow file — the same
// text-scan approach workflowBlock uses for a top-level key, scoped one
// level deeper: a step starts at "      - name: <name>" (six spaces, this
// repository's own indent for every step in e2e.yml and restore.yml) and
// runs until the next sibling step at that same indent, or a dedent past
// the steps list entirely.
func workflowStep(t *testing.T, file, name string) string {
	t.Helper()

	normalized := strings.ReplaceAll(workflowFile(t, file), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	start := -1
	for i, line := range lines {
		if line == "      - name: "+name {
			start = i + 1
			break
		}
	}
	require.NotEqual(t, -1, start, "%s must declare a %q step", file, name)

	var block []string
	for _, line := range lines[start:] {
		if strings.HasPrefix(line, "      - ") {
			break // the next step
		}
		if line != "" && !strings.HasPrefix(line, "      ") {
			break // dedented past the steps list
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

// TestE2EImageVerificationGuardsAgainstMultipleDependencies is issue #343:
// the "Verify the image..." step derives app's expected image by
// subtracting db's own image list from app's resolved one, which only
// leaves exactly one line as long as app has no dependency besides db. A
// regression here — dropping the count check, or reverting to trusting
// "non-empty" — would leave `go test ./...` fully green while silently
// reopening #343's actual failure mode: `docker image inspect` receiving a
// multi-line argument and failing with a raw, unhelpful Docker error
// instead of a message naming the extra images.
func TestE2EImageVerificationGuardsAgainstMultipleDependencies(t *testing.T) {
	t.Parallel()

	block := workflowStep(t, ".github/workflows/e2e.yml", "Verify the image Compose will actually use is the one just loaded")

	assert.Contains(t, block, `expected_count="$(printf '%s\n' "$expected" | grep -c .)"`,
		"the step must count how many lines are left after subtracting db's images")
	assert.Contains(t, block, `if [ "$expected_count" -ne 1 ]; then`,
		"the step must fail before docker image inspect when more than one image is left, not just when zero are")
	assert.Contains(t, block, "app likely gained a new dependency beyond db",
		"the failure message must name what changed, not just fail closed silently")
}

// TestE2EHealthzWaitUsesTheComposeNetwork is issue #358: docker-compose.e2e.yml's
// traefik no longer publishes a host port, so this step must reach it by
// compose service name from a container on the project's own network,
// never the runner's own localhost. A regression back to
// `https://localhost:8443` would leave `go test ./...` green and only fail
// once the step actually runs against a traefik with nothing bound to
// publish — a CI failure, not a test failure, and exactly the gap #358's
// own fix closed.
func TestE2EHealthzWaitUsesTheComposeNetwork(t *testing.T) {
	t.Parallel()

	block := workflowStep(t, ".github/workflows/e2e.yml", "Wait for the app to answer through Traefik")

	assert.Contains(t, block, "https://traefik/healthz",
		"the healthz wait must reach traefik by its compose service name")
	assert.NotContains(t, block, "localhost:8443",
		"the healthz wait must never fall back to the host port docker-compose.e2e.yml no longer publishes (#358)")
	assert.Contains(t, block, "--no-deps",
		"the throwaway container running curl must not restart app/db, which are already up")
}

// TestRestoreHealthzWaitsUseTheComposeNetwork is #358's restore.yml half:
// both of this workflow's own healthz waits — before the backup and after
// the restore — must reach traefik the same way e2e.yml's copy does. Two
// separate steps, checked individually so a regression in either names
// itself rather than being masked by the other still passing.
func TestRestoreHealthzWaitsUseTheComposeNetwork(t *testing.T) {
	t.Parallel()

	for _, step := range []string{
		"Wait for the app to answer through Traefik",
		"The restored stack answers /healthz",
	} {
		block := workflowStep(t, ".github/workflows/restore.yml", step)
		assert.Contains(t, block, "https://traefik/healthz",
			"restore.yml's %q step must reach traefik by its compose service name", step)
		assert.NotContains(t, block, "localhost:8443",
			"restore.yml's %q step must never fall back to the host port docker-compose.e2e.yml no longer publishes (#358)", step)
		assert.Contains(t, block, "--no-deps",
			"restore.yml's %q step must not restart app/db via the throwaway curl container", step)
	}
}

// TestE2ENodeModulesCacheHasRestoreKeysFallback is issue #359 item 2: the
// e2e/node_modules cache step must carry a restore-keys fallback under the
// same key prefix, or any e2e/package.json change misses the cache
// completely instead of partially — the exact regression a dropped
// restore-keys line would reintroduce with `go test ./...` staying green.
func TestE2ENodeModulesCacheHasRestoreKeysFallback(t *testing.T) {
	t.Parallel()

	block := workflowStep(t, ".github/workflows/e2e.yml", "Restore e2e/node_modules")

	assert.Contains(t, block, "key: e2e-node-modules-${{ hashFiles('e2e/package.json') }}",
		"the cache's exact key must still hash e2e/package.json")
	assert.Contains(t, block, "restore-keys: |",
		"the cache step must declare a restore-keys fallback")
	assert.Contains(t, block, "            e2e-node-modules-",
		"the restore-keys fallback prefix must share the exact key's own prefix, or it can never match")
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
