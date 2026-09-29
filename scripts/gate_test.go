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
//
// Comments are passed to the stub as an indexed list (STUB_COMMENT_COUNT,
// STUB_COMMENT_1, STUB_COMMENT_2, ...) rather than one joined blob: the
// script makes two different `gh pr view --json comments` calls - a flat
// `.comments[].body` dump for the vote count, and a `... | @base64` one per
// comment for the CHECKS-missing fallback's scoped Suite-line lookup - and
// only a real per-comment boundary lets the stub answer both correctly.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const gateGhStub = `#!/bin/sh
printf 'gh %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  "pr view "*"--json headRefOid,baseRefName"*)
    [ "${STUB_INFO_RC:-0}" = 0 ] || exit "${STUB_INFO_RC}"
    printf '%s\t%s\n' "${STUB_HEAD_SHA:-}" "${STUB_BASE_REF:-integration/harness-welle-2}" ;;
  "pr view "*"--json comments"*"@base64"*)
    i=1
    while [ "$i" -le "${STUB_COMMENT_COUNT:-0}" ]; do
      eval "body=\${STUB_COMMENT_$i}"
      printf '%s' "$body" | base64 -w0
      printf '\n'
      i=$((i + 1))
    done ;;
  "pr view "*"--json comments"*)
    i=1
    while [ "$i" -le "${STUB_COMMENT_COUNT:-0}" ]; do
      eval "body=\${STUB_COMMENT_$i}"
      printf '%s\n' "$body"
      i=$((i + 1))
    done ;;
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

// commentEnv turns a list of full comment bodies into the indexed STUB_* form
// the gh stub reads, each body kept intact (including its own internal blank
// lines) rather than joined with the others.
func commentEnv(bodies ...string) map[string]string {
	env := map[string]string{"STUB_COMMENT_COUNT": strconv.Itoa(len(bodies))}
	for i, b := range bodies {
		env["STUB_COMMENT_"+strconv.Itoa(i+1)] = b
	}
	return env
}

// mergeEnv layers maps left to right; a later map's keys win.
func mergeEnv(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

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
	oldSHA  = "000000000000000000000000000000000000dead"
)

func TestGateThreeApprovesAtHeadMerge(t *testing.T) {
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "integration/harness-welle-2",
	}, commentEnv(
		marker("APPROVE", 2, headSHA, "go"),
		marker("APPROVE", 2, headSHA, "tests"),
		marker("APPROVE", 2, headSHA, "docs"),
	))
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
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		marker("APPROVE", 1, headSHA, "go"),
		marker("APPROVE", 1, headSHA, "docs"),
	))
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
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		marker("BLOCK", 1, headSHA, "go"),
		marker("APPROVE", 1, headSHA, "tests"),
		marker("APPROVE", 1, headSHA, "docs"),
	))
	r := runGate(t, env, "gate", "42")
	if r.exit != 4 {
		t.Fatalf("expected exit 4, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "BLOCK go", "stdout")
}

// A stale APPROVE for a commit that is no longer the head must never count -
// not even to cancel out a fresh BLOCK for the current head. The stale
// marker carries a *higher* round than the fresh ones on purpose: a sha
// filter applied before the round is computed drops it and correctly finds
// round 1 (BLOCK) for the head sha; a filter applied only after computing
// the round across all markers would instead find round 2 globally, see no
// head-sha marker at round 2, and report WAIT rather than BLOCK - so this is
// the case that actually distinguishes the two orders, not just the same
// round with a different sha.
func TestGateStaleApproveDoesNotOutvoteFreshBlock(t *testing.T) {
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		marker("APPROVE", 2, oldSHA, "go"), // stale: a different, earlier commit
		marker("BLOCK", 1, headSHA, "go"),
		marker("APPROVE", 1, headSHA, "tests"),
		marker("APPROVE", 1, headSHA, "docs"),
	))
	r := runGate(t, env, "gate", "42")
	if r.exit != 4 {
		t.Fatalf("expected exit 4 (BLOCK), got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "go: BLOCK r1 @a1b2c3d", "stdout")
	mustContain(t, r.stdout, "BLOCK go", "stdout")
}

// The reviewer that never posted a marker for the current head at all - not
// even a fresh one - is the case review-tests found untested (round 1): it
// must be reported as stale, and it must count toward WAIT, not silently
// read as approved because some marker with its name exists somewhere in the
// thread.
func TestGateStaleOnlyReviewerReportsStaleAndWaits(t *testing.T) {
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		marker("APPROVE", 1, oldSHA, "go"), // go never re-reviewed the new head
		marker("APPROVE", 1, headSHA, "tests"),
		marker("APPROVE", 1, headSHA, "docs"),
	))
	r := runGate(t, env, "gate", "42")
	if r.exit != 3 {
		t.Fatalf("expected exit 3 (WAIT), got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "go: stale APPROVE r1 @"+oldSHA[:7], "stdout names the stale marker")
	mustContain(t, r.stdout, "WAIT go", "stdout")
}

// If only one reviewer has posted at the new round and the other two are
// still sitting on an earlier round's verdict for the very same commit, the
// PR is not merge-ready: the round only really "advances" once all three
// have weighed in on it, so the laggards must show as stale (not approved)
// and the gate must WAIT on them rather than merge on the one fresh verdict.
func TestGateLaggingReviewersAtOldRoundAreStale(t *testing.T) {
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		marker("APPROVE", 1, headSHA, "tests"),
		marker("APPROVE", 1, headSHA, "docs"),
		marker("APPROVE", 2, headSHA, "go"),
	))
	r := runGate(t, env, "gate", "42")
	if r.exit != 3 {
		t.Fatalf("expected exit 3 (WAIT), got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "go: APPROVE r2 @a1b2c3d", "stdout")
	mustContain(t, r.stdout, "tests: stale APPROVE r1 @a1b2c3d", "stdout")
	mustContain(t, r.stdout, "docs: stale APPROVE r1 @a1b2c3d", "stdout")
	mustContain(t, r.stdout, "WAIT tests docs", "stdout")
}

// The very first gh call (fetching the head sha and base) failing - a
// network blip, an expired auth token - must fail closed with exit 2, not
// proceed with an empty head sha that then silently matches nothing.
func TestGateInitialLookupFailureExits2(t *testing.T) {
	r := runGate(t, map[string]string{"STUB_INFO_RC": "1"}, "gate", "42")
	if r.exit != 2 {
		t.Fatalf("expected exit 2, got %d\n%s", r.exit, r.stdout)
	}
	if r.called("--json comments") {
		t.Errorf("must not go on to fetch comments after the initial lookup failed:\n%s", r.calls)
	}
}

// A round-2 APPROVE for the same commit overrides a round-1 BLOCK: the
// current round is the highest round seen for the head sha, so round 1 is
// history the moment round 2 exists for it.
func TestGateRound2OverridesRound1BlockForSameSHA(t *testing.T) {
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		marker("BLOCK", 1, headSHA, "go"),
		marker("APPROVE", 2, headSHA, "go"),
		marker("APPROVE", 2, headSHA, "tests"),
		marker("APPROVE", 2, headSHA, "docs"),
	))
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
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		"## Go Review — VERDICT: APPROVE\n\nLooks fine, no findings.",
		"just a reply from a human, ignore me",
		"<!-- verdict: APPROVE round=1 sha="+headSHA+" -->", // malformed: no reviewer=
		marker("APPROVE", 1, headSHA, "go"),
		marker("APPROVE", 1, headSHA, "tests"),
		marker("APPROVE", 1, headSHA, "docs"),
	))
	r := runGate(t, env, "gate", "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "MERGE", "stdout")
}

// A marker whose sha= is not exactly 40 lowercase hex characters - truncated,
// as a regressed reviewer prompt might emit - must abort loudly (exit 2)
// rather than be silently dropped and leave the reviewer looking stale
// forever (#359).
func TestGateMalformedMarkerShaAborts(t *testing.T) {
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		"<!-- verdict: APPROVE round=1 sha=a1b2c3d reviewer=go -->", // truncated sha
		marker("APPROVE", 1, headSHA, "tests"),
		marker("APPROVE", 1, headSHA, "docs"),
	))
	r := runGate(t, env, "gate", "42")
	if r.exit != 2 {
		t.Fatalf("expected exit 2, got %d\nstdout:\n%s\nstderr:\n%s", r.exit, r.stdout, r.stderr)
	}
	mustContain(t, r.stderr, "malformed verdict marker", "stderr")
	if r.stdout != "" {
		t.Errorf("must not print a verdict summary on a malformed marker, got:\n%s", r.stdout)
	}
}

// Uppercase hex is not a valid marker sha either - git and `gh` both always
// emit lowercase, so anything else is itself evidence of a malformed marker.
func TestGateUppercaseMarkerShaAborts(t *testing.T) {
	upper := strings.ToUpper(headSHA)
	env := mergeEnv(map[string]string{"STUB_HEAD_SHA": headSHA}, commentEnv(
		marker("APPROVE", 1, upper, "go"),
		marker("APPROVE", 1, headSHA, "tests"),
		marker("APPROVE", 1, headSHA, "docs"),
	))
	r := runGate(t, env, "gate", "42")
	if r.exit != 2 {
		t.Fatalf("expected exit 2, got %d\nstdout:\n%s\nstderr:\n%s", r.exit, r.stdout, r.stderr)
	}
	mustContain(t, r.stderr, "malformed verdict marker", "stderr")
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

// approvedComments builds three separate comments (go, tests, docs), each
// ending in its own marker as a real posted comment would. suiteLine, when
// not empty, becomes part of the *tests* comment specifically - never a
// separately joined string - because the production script only trusts a
// Suite line that lives in the same comment as that round's own tests marker.
func approvedComments(round int, sha, suiteLine string) []string {
	tests := marker("APPROVE", round, sha, "tests")
	if suiteLine != "" {
		tests = suiteLine + "\n" + tests
	}
	return []string{
		marker("APPROVE", round, sha, "go"),
		tests,
		marker("APPROVE", round, sha, "docs"),
	}
}

func TestGateChecksPassMergesOnMain(t *testing.T) {
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "pass",
	}, commentEnv(approvedComments(1, headSHA, "")...))
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
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "pending",
	}, commentEnv(approvedComments(1, headSHA, "")...))
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS pending", "stdout")
}

func TestGateChecksFailedBlocks(t *testing.T) {
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "fail",
	}, commentEnv(approvedComments(1, headSHA, "")...))
	r := runGate(t, env, "gate", "7")
	if r.exit != 4 {
		t.Fatalf("expected exit 4, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS failed", "stdout")
}

// No check was ever reported, but the diff is documentation-only and the test
// reviewer's own current-round comment carries a local Suite line: D7's
// fallback is satisfied.
func TestGateChecksMissingDocsOnlyLocalSuiteMerges(t *testing.T) {
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "",
		"STUB_DIFF_FILES": strings.Join([]string{
			"docs/specs/38-release-pipeline-and-nas-runner.md",
			"CLAUDE.md",
		}, "\n"),
	}, commentEnv(approvedComments(1, headSHA, "**Suite:** `docker compose run --rm app go test ./...` → exit 0, 34 passed")...))
	r := runGate(t, env, "gate", "7")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "MERGE", "stdout")
}

// The same docs-only diff, but the test reviewer's current-round comment
// cites a CI run URL (no local exit code) rather than a local run: the
// fallback is not satisfied, and the gate says so instead of merging on
// faith.
func TestGateChecksMissingCIFallbackSuiteDoesNotMerge(t *testing.T) {
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA":    headSHA,
		"STUB_BASE_REF":    "main",
		"STUB_BUCKET":      "",
		"STUB_HEAD_BRANCH": "integration/harness-welle-2",
		"STUB_DIFF_FILES":  "docs/specs/38-release-pipeline-and-nas-runner.md",
	}, commentEnv(approvedComments(1, headSHA, "**Suite:** https://github.com/CDRO/Inventory/actions/runs/123 (success)")...))
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS missing", "stdout")
	mustContain(t, r.stdout, "scripts/dev ci-status "+headSHA, "stdout names the dispatch recipe")
}

// A round-1 local Suite line is real, but it is not *this* round's evidence:
// once the PR has moved to round 2 (say tests re-ran against CI instead),
// the stale round-1 line must not let a CI-only round 2 sneak through the
// fallback. This is the exact bug review-go and review-tests both found in
// round 1, reproduced as a regression test.
func TestGateChecksMissingIgnoresAnOlderRoundsLocalSuiteLine(t *testing.T) {
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA":    headSHA,
		"STUB_BASE_REF":    "main",
		"STUB_BUCKET":      "",
		"STUB_HEAD_BRANCH": "integration/harness-welle-2",
		"STUB_DIFF_FILES":  "docs/specs/38-release-pipeline-and-nas-runner.md",
	}, commentEnv(
		// Round 1: tests ran locally (this must be ignored - it is stale).
		marker("APPROVE", 1, headSHA, "go"),
		"**Suite:** `docker compose run --rm app go test ./...` → exit 0\n"+marker("APPROVE", 1, headSHA, "tests"),
		marker("APPROVE", 1, headSHA, "docs"),
		// Round 2: the current round's tests comment relies on CI instead.
		marker("APPROVE", 2, headSHA, "go"),
		"**Suite:** https://github.com/CDRO/Inventory/actions/runs/999 (success)\n"+marker("APPROVE", 2, headSHA, "tests"),
		marker("APPROVE", 2, headSHA, "docs"),
	))
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3 (fail closed on the stale round-1 line), got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS missing", "stdout")
}

// A non-documentation file in the diff means the fallback never applies, no
// matter what the Suite line says.
func TestGateChecksMissingNonDocDiffDoesNotMerge(t *testing.T) {
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "",
		"STUB_DIFF_FILES": strings.Join([]string{
			"docs/specs/38-release-pipeline-and-nas-runner.md",
			"internal/store/batches.go",
		}, "\n"),
	}, commentEnv(approvedComments(1, headSHA, "**Suite:** `docker compose run --rm app go test ./...` → exit 0")...))
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS missing", "stdout")
}

// If `git diff` itself cannot answer (no such ref, no repo), that is not
// evidence of a documentation-only diff - it must fail closed, not merge.
func TestGateChecksMissingGitFailureDoesNotMerge(t *testing.T) {
	env := mergeEnv(map[string]string{
		"STUB_HEAD_SHA": headSHA,
		"STUB_BASE_REF": "main",
		"STUB_BUCKET":   "",
		"STUB_DIFF_RC":  "128",
	}, commentEnv(approvedComments(1, headSHA, "**Suite:** `docker compose run --rm app go test ./...` → exit 0")...))
	r := runGate(t, env, "gate", "7")
	if r.exit != 3 {
		t.Fatalf("expected exit 3 (fail closed), got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stdout, "CHECKS missing", "stdout")
}
