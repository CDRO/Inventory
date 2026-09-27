package scripts

// scripts/dev.d/gate reaches the outside world through gh and git. gh is
// stubbed the same way dev_test.go stubs it: a recording stub answers each
// call from STUB_* variables. git is stubbed too - only the CHECKS-missing
// path (an empty `pr checks` bucket) ever calls it, to decide whether a diff
// is documentation-only, and stubbing it keeps that path as fast and
// deterministic as everything else here rather than standing up a real
// repository with real commits just to exercise one case (the pattern
// deploy/synology/update_test.go already uses for its own third command,
// git).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const gateGhStub = `#!/bin/sh
printf 'gh %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  "pr view "*"--json headRefOid,baseRefName"*)
    printf '%s\t%s\n' "${STUB_HEAD_SHA:-}" "${STUB_BASE_REF:-integration/harness-welle-2}" ;;
  "pr view "*"--json comments"*)
    printf '%s' "${STUB_COMMENTS:-}" ;;
  "pr view "*"--json headRefName"*)
    printf '%s\n' "${STUB_HEAD_BRANCH:-spec/99-x}" ;;
  "pr checks "*)
    [ -z "${STUB_BUCKET:-}" ] || printf '%s\n' "${STUB_BUCKET}" ;;
esac
exit 0
`

const gateGitStub = `#!/bin/sh
printf 'git %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  "diff --name-only "*)
    [ -z "${STUB_DIFF_FILES:-}" ] || printf '%s\n' "${STUB_DIFF_FILES}"
    exit ${STUB_DIFF_RC:-0} ;;
esac
exit 0
`

type gateResult struct {
	exit   int
	stdout string
	stderr string
	calls  string
}

func (r gateResult) called(substr string) bool { return strings.Contains(r.calls, substr) }

// runGate mirrors dev_test.go's run() but with gh/git stubs shaped for the
// calls scripts/dev.d/gate makes, which run() (built for test/vet/ci-status/
// ci-usage) does not answer.
func runGate(t *testing.T, env map[string]string, args ...string) gateResult {
	t.Helper()

	root := t.TempDir()
	dst := filepath.Join(root, "scripts")
	if err := os.MkdirAll(filepath.Join(dst, "dev.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, "dev", filepath.Join(dst, "dev"))
	copyFile(t, filepath.Join("dev.d", "gate"), filepath.Join(dst, "dev.d", "gate"))

	stubs := filepath.Join(root, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"gh": gateGhStub, "git": gateGitStub} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	log := filepath.Join(root, "calls.log")
	vars := map[string]string{
		"PATH":     stubs + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"STUB_LOG": log,
	}
	for k, v := range env {
		vars[k] = v
	}

	cmd := exec.Command("/bin/sh", append([]string{filepath.Join(dst, "dev")}, args...)...)
	cmd.Env = make([]string, 0, len(vars))
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("running scripts/dev gate: %v", err)
		}
	}

	calls, _ := os.ReadFile(log)
	r := gateResult{exit: exit, stdout: stdout.String(), stderr: stderr.String(), calls: string(calls)}
	t.Logf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s--- calls ---\n%s", r.exit, r.stdout, r.stderr, r.calls)
	return r
}

func marker(verdict string, round int, sha, reviewer string) string {
	return fmt.Sprintf("<!-- verdict: %s round=%d sha=%s reviewer=%s -->", verdict, round, sha, reviewer)
}

const (
	headSHA = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	oldSHA  = "0000000000000000000000000000000000dead"
)

func TestGateThreeApprovesAtHeadMerge(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "integration/harness-welle-2",
		"STUB_COMMENTS": strings.Join([]string{
			marker("APPROVE", 2, headSHA, "go"),
			marker("APPROVE", 2, headSHA, "tests"),
			marker("APPROVE", 2, headSHA, "docs"),
		}, "\n"),
	}
	r := runGate(t, env, "gate", "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\nstdout:\n%s\nstderr:\n%s", r.exit, r.stdout, r.stderr)
	}
	for _, want := range []string{"go: APPROVE r2 @a1b2c3d", "tests: APPROVE r2 @a1b2c3d", "docs: APPROVE r2 @a1b2c3d", "MERGE"} {
		mustContain(t, r.stdout, want, "stdout")
	}
}

// The acceptance criteria's literal example: two APPROVE + one missing verdict
// for the head commit -> "WAIT tests" exit 3.
func TestGateMissingReviewerWaits(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_COMMENTS": strings.Join([]string{
			marker("APPROVE", 1, headSHA, "go"),
			marker("APPROVE", 1, headSHA, "docs"),
		}, "\n"),
	}
	r := runGate(t, env, "gate", "42")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "tests: missing", "stdout")
	mustContain(t, r.stdout, "WAIT tests", "stdout")
}

// The acceptance criteria's literal example: a BLOCK in the current round ->
// "BLOCK go" exit 4, regardless of the other two reviewers.
func TestGateBlockInCurrentRoundBlocks(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_COMMENTS": strings.Join([]string{
			marker("BLOCK", 1, headSHA, "go"),
			marker("APPROVE", 1, headSHA, "tests"),
			marker("APPROVE", 1, headSHA, "docs"),
		}, "\n"),
	}
	r := runGate(t, env, "gate", "42")
	if r.exit != 4 {
		t.Fatalf("expected exit 4, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "BLOCK go", "stdout")
}

// A stale APPROVE for a commit that is no longer the head must never count -
// not even to cancel out a fresh BLOCK for the current head.
func TestGateStaleApproveDoesNotOutvoteFreshBlock(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_COMMENTS": strings.Join([]string{
			marker("APPROVE", 1, oldSHA, "go"), // stale: a different, earlier commit
			marker("BLOCK", 1, headSHA, "go"),
			marker("APPROVE", 1, headSHA, "tests"),
			marker("APPROVE", 1, headSHA, "docs"),
		}, "\n"),
	}
	r := runGate(t, env, "gate", "42")
	if r.exit != 4 {
		t.Fatalf("expected exit 4 (BLOCK), got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "BLOCK go", "stdout")
	mustNotContain(t, r.stdout, oldSHA[:7], "the stale commit must never be cited as current")
}

// A round-2 APPROVE for the same commit overrides a round-1 BLOCK: the
// current round is the highest round seen for the head sha, so round 1 is
// history the moment round 2 exists for it.
func TestGateRound2OverridesRound1BlockForSameSHA(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_COMMENTS": strings.Join([]string{
			marker("BLOCK", 1, headSHA, "go"),
			marker("APPROVE", 2, headSHA, "go"),
			marker("APPROVE", 2, headSHA, "tests"),
			marker("APPROVE", 2, headSHA, "docs"),
		}, "\n"),
	}
	r := runGate(t, env, "gate", "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0 (MERGE), got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "go: APPROVE r2 @a1b2c3d", "stdout")
	mustContain(t, r.stdout, "MERGE", "stdout")
}

// Comments that carry no marker line - ordinary prose, a reply, a malformed
// near-miss - must be invisible to the gate.
func TestGateIgnoresCommentsWithoutAMarker(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_COMMENTS": strings.Join([]string{
			"## Go Review — VERDICT: APPROVE\n\nLooks fine, no findings.",
			"just a reply from a human, ignore me",
			"<!-- verdict: APPROVE round=1 sha=" + headSHA + " -->", // malformed: no reviewer=
			marker("APPROVE", 1, headSHA, "go"),
			marker("APPROVE", 1, headSHA, "tests"),
			marker("APPROVE", 1, headSHA, "docs"),
		}, "\n"),
	}
	r := runGate(t, env, "gate", "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "MERGE", "stdout")
}

func TestGateUsageRequiresANumericPR(t *testing.T) {
	for _, bad := range []string{"", "abc", "12x"} {
		var args []string
		if bad != "" {
			args = []string{"gate", bad}
		} else {
			args = []string{"gate"}
		}
		r := runGate(t, nil, args...)
		if r.exit != 2 {
			t.Errorf("%q: expected exit 2, got %d", bad, r.exit)
		}
		if r.calls != "" {
			t.Errorf("%q: must not call gh, but called:\n%s", bad, r.calls)
		}
	}
}

func TestGateHelpExitsZeroAndCallsNothing(t *testing.T) {
	r := runGate(t, nil, "gate", "--help")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.stdout, "scripts/dev gate", "stdout names its own usage")
	if r.calls != "" {
		t.Errorf("--help must not call gh, but called:\n%s", r.calls)
	}
}

// --- CHECKS (decision D7) --------------------------------------------------

func approvedAtRound(round int, sha string) string {
	return strings.Join([]string{
		marker("APPROVE", round, sha, "go"),
		marker("APPROVE", round, sha, "tests"),
		marker("APPROVE", round, sha, "docs"),
	}, "\n")
}

func TestGateChecksPassMergesOnMain(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_COMMENTS": approvedAtRound(1, headSHA),
		"STUB_BUCKET":   "pass",
	}
	r := runGate(t, env, "gate", "7")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "MERGE", "stdout")
	if !r.called("pr checks") {
		t.Errorf("a PR against main must consult pr checks, calls:\n%s", r.calls)
	}
}

func TestGateChecksPendingWaits(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_COMMENTS": approvedAtRound(1, headSHA),
		"STUB_BUCKET":   "pending",
	}
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS pending", "stdout")
}

func TestGateChecksFailedBlocks(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_COMMENTS": approvedAtRound(1, headSHA),
		"STUB_BUCKET":   "fail",
	}
	r := runGate(t, env, "gate", "7")
	if r.exit != 4 {
		t.Fatalf("expected exit 4, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS failed", "stdout")
}

// No check was ever reported, but the diff is documentation-only and the test
// reviewer's own Suite line shows a local run: D7's fallback is satisfied.
func TestGateChecksMissingDocsOnlyLocalSuiteMerges(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "",
		"STUB_DIFF_FILES": strings.Join([]string{
			"docs/specs/38-release-pipeline-and-nas-runner.md",
			"CLAUDE.md",
		}, "\n"),
		"STUB_COMMENTS": approvedAtRound(1, headSHA) + "\n" +
			"**Suite:** `docker compose run --rm app go test ./...` → exit 0, 34 passed",
	}
	r := runGate(t, env, "gate", "7")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "MERGE", "stdout")
}

// The same docs-only diff, but the test reviewer's Suite line cites a CI run
// URL (no local exit code) rather than a local run: the fallback is not
// satisfied, and the gate says so instead of merging on faith.
func TestGateChecksMissingCIFallbackSuiteDoesNotMerge(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA":    headSHA,
		"STUB_BASE_REF":    "main",
		"STUB_BUCKET":      "",
		"STUB_HEAD_BRANCH": "integration/harness-welle-2",
		"STUB_DIFF_FILES":  "docs/specs/38-release-pipeline-and-nas-runner.md",
		"STUB_COMMENTS": approvedAtRound(1, headSHA) + "\n" +
			"**Suite:** https://github.com/CDRO/Inventory/actions/runs/123 (success)",
	}
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS missing", "stdout")
	mustContain(t, r.stdout, "scripts/dev ci-status "+headSHA, "stdout names the dispatch recipe")
}

// A non-documentation file in the diff means the fallback never applies, no
// matter what the Suite line says.
func TestGateChecksMissingNonDocDiffDoesNotMerge(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "",
		"STUB_DIFF_FILES": strings.Join([]string{
			"docs/specs/38-release-pipeline-and-nas-runner.md",
			"internal/store/batches.go",
		}, "\n"),
		"STUB_COMMENTS": approvedAtRound(1, headSHA) + "\n" +
			"**Suite:** `docker compose run --rm app go test ./...` → exit 0",
	}
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS missing", "stdout")
}

// If `git diff` itself cannot answer (no such ref, no repo), that is not
// evidence of a documentation-only diff - it must fail closed, not merge.
func TestGateChecksMissingGitFailureDoesNotMerge(t *testing.T) {
	env := map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "",
		"STUB_DIFF_RC":  "128",
		"STUB_COMMENTS": approvedAtRound(1, headSHA) + "\n" +
			"**Suite:** `docker compose run --rm app go test ./...` → exit 0",
	}
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3 (fail closed), got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS missing", "stdout")
}
