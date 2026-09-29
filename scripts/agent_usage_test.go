// Tests for scripts/dev.d/agent-usage (issue #308, H9). Fixture transcripts
// live under scripts/testdata/agent-usage/ in the exact shape
// `~/.claude/projects/<slug>/**.jsonl` has: a project-slug directory per
// worktree, subagent transcripts under `<session>/subagents/`, each paired
// with a `.meta.json`. AGENT_USAGE_PROJECTS_DIR points the command at that
// tree instead of the real `~/.claude/projects`, and AGENT_USAGE_REPONAME
// stands in for the git-derived repo name (the throwaway checkout `run()`
// builds has no `.git` common dir to read it from) - the two fixture
// directories use non-overlapping name fragments ("testrepo" and "prscope")
// so a single AGENT_USAGE_REPONAME selects exactly one of them, and tests
// never have to copy fixtures around to scope a run.
package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func agentUsageProjectsDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("testdata/agent-usage")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func agentUsageEnv(t *testing.T, reponame string, extra map[string]string) map[string]string {
	env := map[string]string{
		"AGENT_USAGE_PROJECTS_DIR": agentUsageProjectsDir(t),
		"AGENT_USAGE_REPONAME":     reponame,
		"HOME":                     t.TempDir(), // must not be read when the override is set
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// row matches one line of the table: model, role, calls, then the five
// numeric columns in the order the header prints them (input, cache_creation,
// cache_read, output, max_ctx), letting a test omit median_ctx when a group's
// exact median isn't the point.
func row(t *testing.T, stdout, model, role string, calls int, rest ...string) {
	t.Helper()
	pattern := `(?m)^` + regexp.QuoteMeta(model) + `\s+` + regexp.QuoteMeta(role) + `\s+` + regexp.QuoteMeta(itoa(calls))
	for _, r := range rest {
		pattern += `\s+` + regexp.QuoteMeta(r)
	}
	re := regexp.MustCompile(pattern)
	if !re.MatchString(stdout) {
		t.Errorf("expected a row matching %s in:\n%s", pattern, stdout)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// --- the three traps --------------------------------------------------

// TestAgentUsageTrap1DropsInterimSnapshotWithoutOutputTokens: msg_trap1 in
// the fixture has an interim line with no `output_tokens` key at all, then a
// final line with output=20. Only the final line's numbers may appear.
//
// msg_trap1_only_interim, also on 2026-09-24, goes further: it has NO line
// that ever carries `output_tokens` for its id - a session that streamed a
// partial response and was cut off before the final flush. Without the
// trap-1 guard (`if (out == "") next` in scripts/dev.d/agent-usage), this
// line's in/cc/cr (555/555/555) would still land in `best_*` as a third call
// (out coerces to 0, but input/cache figures would not), changing both the
// call count and the sums below - so the exact row this test pins already
// proves it contributes nothing; commenting out the guard makes this test
// fail (verified by hand: reverting the guard turns "calls 2" into "calls
// 3" and inflates the input/cache_creation/cache_read sums by 555 each).
func TestAgentUsageTrap1DropsInterimSnapshotWithoutOutputTokens(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2026-09-24", "--until", "2026-09-24")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	// Only trap1 (ctx=172) and trap2 (ctx=100) fall on 2026-09-24; trap1's
	// interim snapshot (in=2,cc=100,cr=50, no output) would double the input
	// and cache figures if it were ever counted alongside the final one.
	// Two calls -> an even-count median: (100+172)/2 = 136.0; max is the
	// larger, 172.
	row(t, r.stdout, "claude-sonnet-5", "main", 2, "12", "120", "80", "60", "172", "136.0")
	mustNotContain(t, r.stdout, "555", "msg_trap1_only_interim (no line ever carries output_tokens) must contribute nothing to any total")
}

// TestAgentUsageTrap2TakesTheLastUsageObjectOnTheLine: msg_trap2's line
// carries two decoys - a whole `"usage":{...}` object (input=999 etc, nested
// under "iterations_preview") before the real one, and, *inside* the real
// object, an `iterations` array whose own input/output (777/888) differ from
// the object's own top-level fields (input=10,cc=20,cr=30,out=40). Neither
// decoy's values may reach the total: the first proves the last "usage":{
// on the line is chosen over an earlier one; the second (777/888 not
// appearing) proves each field is read at its *first* occurrence within that
// chosen object, not wherever a later, coincidentally-matching-if-equal
// nested copy happens to be - the two decoys used to share the same values
// as the real fields, which meant this test could not tell first-occurrence
// from last-occurrence apart (review-go, PR #353 round 1).
func TestAgentUsageTrap2TakesTheLastUsageObjectOnTheLine(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2026-09-24", "--until", "2026-09-24", "--json")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, `"input":12`, "stdout") // 2 (trap1) + 10 (trap2's real usage, not 999 or 777)
	mustNotContain(t, r.stdout, "999", "the decoy usage object's values must never appear")
	mustNotContain(t, r.stdout, "777", "the nested iterations array's input_tokens must never appear")
	mustNotContain(t, r.stdout, "888", "the nested iterations array's output_tokens must never appear")
}

// TestAgentUsageTrap3DedupesByMessageIDKeepingTheFinalBlock: msg_trap3
// appears on three lines - an early content block (in=1,out=1), the final
// block (in=5,out=8), and a byte-for-byte duplicate of the final block. All
// three share one message id; only one call's worth of tokens may be
// counted, and it must be the final block's numbers, not the first block's
// nor a doubled final block.
func TestAgentUsageTrap3DedupesByMessageIDKeepingTheFinalBlock(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2026-09-25", "--until", "2026-09-25")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	// Only msg_trap3 falls on 2026-09-25 in the top-level session file, plus
	// the two subagent calls (different files, own message ids, always kept
	// separately) - review-go (ctx=18) and the meta.json-named general-purpose
	// call (ctx=34).
	// A single call -> its own context is both the max and the median.
	row(t, r.stdout, "claude-sonnet-5", "main", 1, "5", "6", "7", "8", "26", "26.0")
}

// TestAgentUsageSyntheticModelRowsAreFiltered: msg_synthetic_compaction
// (2026-09-26) carries `"model":"<synthetic>"` - Claude Code's own internal
// placeholder record (e.g. a compaction marker), legitimate and zero-cost,
// not a parsing bug. It must not appear as its own row at all, rather than
// showing up as a noisy all-zero-token row with calls=1.
func TestAgentUsageSyntheticModelRowsAreFiltered(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2026-09-26", "--until", "2026-09-26")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "(no calls in range)", "the only record in range is a <synthetic> placeholder and must not produce a row")
	mustNotContain(t, r.stdout, "synthetic", "stdout")
}

// --- role: attributionAgent, the meta.json fallback, and "main" -------

func TestAgentUsageRoleFromAttributionAgentAndMetaJSONFallback(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2026-09-25", "--until", "2026-09-25")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	// agent-review-go.jsonl carries attributionAgent="review-go" directly.
	row(t, r.stdout, "claude-sonnet-5", "review-go", 1, "3", "4", "5", "6", "18", "18.0")
	// agent-unnamed.jsonl has no attributionAgent field; its sibling
	// .meta.json's agentType ("general-purpose") is the fallback.
	row(t, r.stdout, "claude-opus-5", "general-purpose", 1, "7", "8", "9", "10", "34", "34.0")
}

// --- --since / --until --------------------------------------------------

func TestAgentUsageSinceUntilExcludesOutOfRangeCalls(t *testing.T) {
	withRange := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2026-09-24", "--until", "2026-09-26")
	mustNotContain(t, withRange.stdout, "400", "msg_old (2026-08-01, ctx 400) is outside --since/--until and must not appear")

	// Without a range, all four calls in (claude-sonnet-5, main) are in
	// scope: contexts [26, 100, 172, 400] (msg_old's 400 included) -> an
	// even-count median of (100+172)/2 = 136.0, max 400.
	noRange := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage")
	row(t, noRange.stdout, "claude-sonnet-5", "main", 4, "117", "226", "187", "168", "400", "136.0")
}

// TestAgentUsageMedianIsExactOnAnOddCount: the "prscope" fixtures'
// (claude-sonnet-5, main) group has three calls - contexts 50, 90 and 4000 -
// an odd count, so the median must be the exact middle value (90), not an
// average of two neighbors the way the even-count cases above are. A
// off-by-one in the (cnt+1)/2 index would silently return 50 or 4000 instead.
func TestAgentUsageMedianIsExactOnAnOddCount(t *testing.T) {
	r := run(t, agentUsageEnv(t, "prscope", nil), "agent-usage")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	row(t, r.stdout, "claude-sonnet-5", "main", 3, "1032", "1034", "1036", "1038", "4000", "90.0")
}

func TestAgentUsageRejectsMalformedDates(t *testing.T) {
	for _, bad := range []string{"2026-9-1", "09-2026-01", "not-a-date"} {
		if r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", bad); r.exit != 2 {
			t.Errorf("--since %q: expected exit 2, got %d", bad, r.exit)
		}
	}
}

// --- --pr: gitBranch, and the cwd fallback for a detached/HEAD record -----

const prGhStub = `#!/bin/sh
if [ "${STUB_GH_FAIL:-}" = "1" ]; then
  echo "GraphQL: Could not resolve to a PullRequest with the number of 999999. (repository.pullRequest)" >&2
  exit 1
fi
case "$*" in
  "pr view "*"--json headRefName -q .headRefName")
    printf '%s\n' "$STUB_PR_BRANCH"; exit 0 ;;
esac
exit 1
`

func writeGhStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gh-stub.sh")
	if err := os.WriteFile(path, []byte(prGhStub), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAgentUsagePRFiltersByGitBranchWithCwdFallback: three fixture calls on
// the "prscope" repo - one on the PR's own branch (gitBranch set), one on
// "main" (must be excluded), and one recorded with gitBranch "HEAD" whose cwd
// is the PR branch's own worktree (must be included via the cwd fallback,
// since a detached/HEAD record cannot be matched by gitBranch alone).
func TestAgentUsagePRFiltersByGitBranchWithCwdFallback(t *testing.T) {
	env := agentUsageEnv(t, "prscope", map[string]string{
		"AGENT_USAGE_GH": writeGhStub(t),
		"STUB_PR_BRANCH": "feature/target-branch",
	})
	r := run(t, env, "agent-usage", "--pr", "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustNotContain(t, r.stdout, "1000", "session-other.jsonl (branch=main) must be excluded from the PR total")
	// Two calls in scope (ctx 50 and 90) -> even-count median (50+90)/2=70.0.
	row(t, r.stdout, "claude-sonnet-5", "main", 2, "32", "34", "36", "38", "90", "70.0")
}

// TestAgentUsagePRCwdFallbackIsASuffixMatchNotASubstringOne: the "cwdscope"
// fixture's cwd is ".../Testrepo-target-branch-old" - it contains the
// branch's suffix ("target-branch") but does not end in it, so it must be
// excluded from the --pr total (an earlier version used a substring-anywhere
// match, which this fixture would have passed).
func TestAgentUsagePRCwdFallbackIsASuffixMatchNotASubstringOne(t *testing.T) {
	env := agentUsageEnv(t, "cwdscope", map[string]string{
		"AGENT_USAGE_GH": writeGhStub(t),
		"STUB_PR_BRANCH": "feature/target-branch",
	})
	r := run(t, env, "agent-usage", "--pr", "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "(no calls in range)", "a cwd that merely contains the branch suffix, without ending in it, must not match")
}

// TestAgentUsagePRCwdFallbackRequiresBoundaryBeforeSuffix: the "boundaryscope"
// fixture has two calls, both genuinely ending in the branch suffix "test" as
// a plain string - one at ".../Repo-unittest" (the match is the tail of a
// longer word, "unittest", with no separator before "test") and one at
// ".../Repo-test" (hyphen-anchored). Only the second is a real suffix-of-
// path-segment match; the first is the same class of false positive the
// round-1 substring-anywhere fix addressed, one level more precise - suffix-
// of-string is not suffix-of-path-segment. Reverting the boundary check in
// ends_with() (scripts/dev.d/agent-usage) makes both match, pulling "6000"
// into the total.
func TestAgentUsagePRCwdFallbackRequiresBoundaryBeforeSuffix(t *testing.T) {
	env := agentUsageEnv(t, "boundaryscope", map[string]string{
		"AGENT_USAGE_GH": writeGhStub(t),
		"STUB_PR_BRANCH": "feature/test",
	})
	r := run(t, env, "agent-usage", "--pr", "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustNotContain(t, r.stdout, "6000", "a cwd ending in the suffix with no boundary before it (.../Repo-unittest) must not match")
	row(t, r.stdout, "claude-sonnet-5", "main", 1, "7000", "7000", "7000", "7000")
}

// TestAgentUsagePRLookupFailureExitsNonZero also pins that `gh`'s own error
// text reaches stderr (review-go, PR #353 round 1: an auth failure, a
// network error and "no such PR" used to all produce the identical generic
// message, since gh's stderr was discarded with 2>/dev/null).
func TestAgentUsagePRLookupFailureExitsNonZero(t *testing.T) {
	env := agentUsageEnv(t, "prscope", map[string]string{
		"AGENT_USAGE_GH": writeGhStub(t),
		"STUB_GH_FAIL":   "1",
	})
	r := run(t, env, "agent-usage", "--pr", "999999")
	if r.exit == 0 {
		t.Fatalf("expected a non-zero exit when the PR's branch cannot be resolved")
	}
	mustContain(t, r.stderr, "999999", "stderr names the PR that failed")
	mustContain(t, r.stderr, "Could not resolve to a PullRequest", "gh's own error text must reach stderr, not just a generic message")
}

func TestAgentUsageRejectsNonNumericPR(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--pr", "abc")
	if r.exit != 2 {
		t.Fatalf("expected exit 2, got %d", r.exit)
	}
}

// --- never prints message content -----------------------------------------

func TestAgentUsageNeverPrintsMessageContent(t *testing.T) {
	for _, args := range [][]string{
		{"agent-usage"},
		{"agent-usage", "--json"},
	} {
		r := run(t, agentUsageEnv(t, "testrepo", nil), args...)
		mustNotContain(t, r.stdout, "SECRET-MARKER-DO-NOT-LEAK", strings.Join(args, " ")+": stdout must carry counts only, never message text")
	}
}

// --- --json ----------------------------------------------------------------

func TestAgentUsageJSONShapeAndTotal(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2026-09-25", "--until", "2026-09-25", "--json")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	for _, want := range []string{
		`"rows": [`, `"total": {`,
		`"model":"claude-sonnet-5"`, `"role":"review-go"`, `"calls":1`,
		`"model":"claude-opus-5"`, `"role":"general-purpose"`,
	} {
		mustContain(t, r.stdout, want, "stdout")
	}
	if strings.Count(r.stdout, "{") != strings.Count(r.stdout, "}") {
		t.Errorf("unbalanced braces in JSON output:\n%s", r.stdout)
	}
}

// TestAgentUsageJSONPinsMaxAndMedianContextByKey: the review-go row on
// 2026-09-25 is a single call (in=3,cc=4,cr=5,out=6 -> context 18), so its
// max_context and median_context are both 18/18.0. Checking the two fields
// together, in the order flushgrp()'s printf actually emits them, is the
// point: swapping their order in that printf (scripts/dev.d/agent-usage)
// left the whole suite green before this test existed, since nothing
// asserted either field by key/value.
func TestAgentUsageJSONPinsMaxAndMedianContextByKey(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2026-09-25", "--until", "2026-09-25", "--json")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, `"output":6,"max_context":18,"median_context":18.0`, "stdout")
}

// TestAgentUsageEmptyRangeProducesNoRows exercises the zero-groups path in
// pass2.awk directly - the same path a busybox-only bug once broke silently
// (a phantom all-zero row from an uninitialized accumulator that compared
// unequal to 0; see the BEGIN block's comment in scripts/dev.d/agent-usage).
func TestAgentUsageEmptyRangeProducesNoRows(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2020-01-01", "--until", "2020-01-02")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "(no calls in range)", "stdout")
	mustNotContain(t, r.stdout, "claude", "no model should ever appear when nothing matched")

	rj := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--since", "2020-01-01", "--until", "2020-01-02", "--json")
	mustContain(t, rj.stdout, `"rows": [`, "stdout")
	mustContain(t, rj.stdout, `"total": {"calls":0,"input":0,"cache_creation":0,"cache_read":0,"output":0}`, "stdout")
	mustNotContain(t, rj.stdout, `"model"`, "an empty rows array must carry no row objects")
}

// --- no matching transcripts -------------------------------------------

func TestAgentUsageNoMatchingProjectDirExitsZero(t *testing.T) {
	r := run(t, agentUsageEnv(t, "no-such-repo-anywhere", nil), "agent-usage")
	if r.exit != 0 {
		t.Fatalf("expected exit 0 for an empty result, got %d", r.exit)
	}
	mustContain(t, r.stderr, "no-such-repo-anywhere", "stderr names what it looked for")
}

// --- help --------------------------------------------------------------

func TestAgentUsageHelpIsTheHeaderAndNothingElse(t *testing.T) {
	r := run(t, agentUsageEnv(t, "testrepo", nil), "agent-usage", "--help")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.stdout, "scripts/dev agent-usage", "--help names its own usage")
	mustNotContain(t, r.stdout, "set -u", "--help must not leak code")
}
