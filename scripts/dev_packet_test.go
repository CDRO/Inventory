// Tests for scripts/dev.d/packet, kept in their own file with their own stubs
// and runner (runPacket, not the dev_test.go run()) rather than extending the
// shared gh/docker stubs in dev_test.go: several other harness-optimization
// packages land scripts/dev.d/<command> tests in this same package in
// parallel (docs/plans/2026-09-harness-optimization.md, wave 2's collision
// notes), and a second command's gh/git call shapes have nothing to do with
// the first's, so a shared stub would just grow one long case block that
// every package's PR touches. packet needs `git` stubbed too, which
// dev_test.go's run() does not do at all (scripts/dev's own commands never
// call git).
//
// mustContain, mustNotContain, copyFile and the result type come from
// dev_test.go - they are generic to any of these command tests, not specific
// to scripts/dev's own stub set.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// packetGhStub answers the five single-field `gh pr view --json <field> -q
// .<field>` calls the script makes, `gh issue view <n> --json body|title`
// per issue (looked up by number so a scenario can serve several, or make
// one fail the way a PR-not-issue reference does), `gh pr checks` and the
// one `gh pr view --json comments` call `--since` mode makes.
const packetGhStub = `#!/bin/sh
printf 'gh %s\n' "$*" >> "$STUB_LOG"
case "$1 $2" in
  "pr view")
    n=$3
    case "$*" in
      *"--json number"*)
        # The script's PR-vs-issue check for a referenced #n: a PR number
        # answers this, a real issue number does not.
        eval "ispr=\${STUB_PR_${n}_EXISTS:-0}"
        [ "$ispr" = "1" ] && { echo "$n"; exit 0; }
        exit 1 ;;
      *"--json title "*) echo "$STUB_TITLE" ;;
      *"--json body "*) printf '%s\n' "$STUB_PRBODY" ;;
      *"--json baseRefName "*) echo "$STUB_BASE" ;;
      *"--json headRefName "*) echo "$STUB_HEADREF" ;;
      *"--json headRefOid "*) echo "$STUB_SHA" ;;
      *"--json comments "*)
        # Actually performs the join the real query asks for, with whatever
        # separator sits between join("...") in the command line, instead of
        # a fixture pre-joined by the test - that was exactly how the round-1
        # test review's finding (join("") losing its separator) went unnoticed:
        # the old fixture was hand-joined with the correct byte regardless of
        # what the real -q expression asked for.
        q="$*"
        rest=${q#*'join("'}
        sep=${rest%%'")'*}
        # The stub never runs real jq, so it sees the query's literal source
        # text, not what jq's own \uXXXX decoding would produce at runtime;
        # this is the one escape the real script relies on, decoded by hand.
        case "$sep" in
          '\u001e') sep=$(printf '\036') ;;
        esac
        out=""
        i=1
        while :; do
          eval "c=\${STUB_COMMENT_${i}:-__PACKET_TEST_UNSET__}"
          [ "$c" = "__PACKET_TEST_UNSET__" ] && break
          if [ -z "$out" ]; then out=$c; else out="$out$sep$c"; fi
          i=$((i + 1))
        done
        printf '%s' "$out" ;;
    esac
    exit 0 ;;
  "pr checks")
    [ -z "${STUB_CHECKS_OUTPUT:-}" ] || printf '%s\n' "$STUB_CHECKS_OUTPUT"
    exit "${STUB_CHECKS_RC:-0}" ;;
  "issue view")
    n=$3
    eval "found=\${STUB_ISSUE_${n}_FOUND:-1}"
    if [ "$found" = "0" ]; then
      echo "issue not found" >&2
      exit 1
    fi
    case "$*" in
      *"--json body "*) eval "printf '%s\n' \"\$STUB_ISSUE_${n}_BODY\"" ;;
      *"--json title "*) eval "echo \"\$STUB_ISSUE_${n}_TITLE\"" ;;
    esac
    exit 0 ;;
esac
exit 0
`

// packetGitStub answers the fetch/rev-parse pre-flight (always says
// origin/<base> exists), the three-dot whole-PR --stat and --name-only, the
// merge-detection `log --merges`, and the two diff shapes: a three-dot range
// ("...") is the whole-PR or merge-fallback diff, anything else (a two-dot
// "since..head" range) is the delta.
const packetGitStub = `#!/bin/sh
printf 'git %s\n' "$*" >> "$STUB_LOG"
case "$1" in
  fetch) exit 0 ;;
  rev-parse) exit 0 ;;
  log)
    [ -z "${STUB_MERGES:-}" ] || printf '%s\n' "$STUB_MERGES"
    exit 0 ;;
  diff)
    case "$*" in
      *"--stat "*) printf '%s\n' "${STUB_DIFF_STAT:-}" ;;
      *"--name-only "*) printf '%s\n' "${STUB_DIFF_NAMES:-}" ;;
      *"..."*) printf '%s\n' "${STUB_DIFF_FULL:-}" ;;
      *) printf '%s\n' "${STUB_DIFF_DELTA:-}" ;;
    esac
    exit 0 ;;
esac
exit 0
`

// runPacket copies scripts/dev.d/packet into a throwaway repository root (the
// script locates its root the same way scripts/dev does, two levels above
// its own path), writes any docs/specs fixtures the scenario needs, writes
// the stubs, and runs the script directly the way deploy/synology/update_test.go
// runs `update` - packet is never invoked through the scripts/dev dispatcher
// here because the dispatcher's own contract is dev_test.go's to cover.
func runPacket(t *testing.T, env map[string]string, specs map[string]string, args ...string) result {
	t.Helper()

	root := t.TempDir()
	dst := filepath.Join(root, "scripts", "dev.d")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join("dev.d", "packet"), filepath.Join(dst, "packet"))

	for path, content := range specs {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stubs := filepath.Join(root, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"gh": packetGhStub, "git": packetGitStub} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	log := filepath.Join(root, "calls.log")
	vars := map[string]string{
		"PATH":     stubs + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"STUB_LOG": log,
		"STUB_DIR": root,
	}
	for k, v := range env {
		vars[k] = v
	}

	cmd := exec.Command("/bin/sh", append([]string{filepath.Join(dst, "packet")}, args...)...)
	cmd.Dir = root
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
			t.Fatalf("running packet: %v", err)
		}
	}

	calls, _ := os.ReadFile(log)
	packetFile, _ := os.ReadFile(filepath.Join(root, ".claude", "review-packet.md"))
	r := result{exit: exit, stdout: stdout.String() + string(packetFile), stderr: stderr.String(), calls: string(calls)}
	t.Logf("exit %d\n--- stderr ---\n%s--- packet ---\n%s", r.exit, r.stderr, packetFile)
	return r
}

func basicEnv() map[string]string {
	return map[string]string{
		"STUB_TITLE":     "H7: review packet",
		"STUB_PRBODY":    "Implements #12.",
		"STUB_BASE":      "integration/harness-welle-2",
		"STUB_HEADREF":   "h7/review-packet",
		"STUB_SHA":       "abc123def456",
		"STUB_DIFF_STAT": " internal/foo/foo.go | 7 +++++++\n 1 file changed, 7 insertions(+)",
	}
}

func TestPacketRejectsMissingOrBadPR(t *testing.T) {
	if r := runPacket(t, nil, nil); r.exit != 2 {
		t.Fatalf("no PR: expected exit 2, got %d", r.exit)
	}
	if r := runPacket(t, nil, nil, "main"); r.exit != 2 {
		t.Fatalf("non-numeric PR: expected exit 2, got %d", r.exit)
	}
	if r := runPacket(t, nil, nil, "12", "34"); r.exit != 2 {
		t.Fatalf("two PRs: expected exit 2, got %d", r.exit)
	}
}

// TestPacketHappyPathWritesEveryTopLevelSection pins the order the issue
// specifies: title/base/head/SHA/body, issue bodies, diff --stat, the diff,
// the exported-identifier table, test files, CI status.
func TestPacketHappyPathWritesEveryTopLevelSection(t *testing.T) {
	env := basicEnv()
	env["STUB_ISSUE_12_FOUND"] = "1"
	env["STUB_ISSUE_12_TITLE"] = "H7: scripts/dev packet"
	env["STUB_ISSUE_12_BODY"] = "Build the packet command."
	env["STUB_DIFF_FULL"] = "diff --git a/internal/foo/foo.go b/internal/foo/foo.go\n" +
		"+++ b/internal/foo/foo.go\n" +
		"@@ -1,1 +1,4 @@\n" +
		" package foo\n" +
		"+\n" +
		"+func Baz() error {\n" +
		"+\treturn nil\n" +
		"+}\n"
	env["STUB_DIFF_NAMES"] = "internal/foo/foo.go\ninternal/foo/foo_test.go"
	env["STUB_CHECKS_OUTPUT"] = "test\tpass\t1m2s"

	r := runPacket(t, env, nil, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}

	for _, want := range []string{
		"PR #42",
		"H7: review packet",
		"Head SHA: abc123def456",
		"Implements #12.",
		"Issue #12: H7: scripts/dev packet",
		"Build the packet command.",
		"internal/foo/foo.go | 7 +++++++",
		"func Baz() error",
		"internal/foo/foo_test.go",
		"test\tpass\t1m2s",
	} {
		mustContain(t, r.stdout, want, "the packet")
	}

	titleIdx := strings.Index(r.stdout, "H7: review packet")
	issueIdx := strings.Index(r.stdout, "Issue #12")
	diffIdx := strings.Index(r.stdout, "## Diff")
	identIdx := strings.Index(r.stdout, "## Exported identifiers")
	testsIdx := strings.Index(r.stdout, "## Test files changed")
	ciIdx := strings.Index(r.stdout, "## CI status")
	if !(titleIdx < issueIdx && issueIdx < diffIdx && diffIdx < identIdx && identIdx < testsIdx && testsIdx < ciIdx) {
		t.Errorf("sections are out of the issue's specified order:\n%s", r.stdout)
	}
}

// The bug the round-1 test review caught on this PR itself: a PR's own body
// routinely mentions its own number (this PR's "Size proof" section names
// #335), and gh issue view resolves a PR number too - it does not fail the
// way a made-up number does - so without excluding $pr up front the packet
// duplicated the whole PR body a second time under a spurious "Issue #335"
// heading, and its actual size on disk (71 181 bytes, 61.5%) blew past the
// 60% acceptance criterion the PR body claimed (60 909 bytes, 52.6%) from a
// number that was never actually written to .claude/review-packet.md.
func TestPacketDoesNotTreatItsOwnPRNumberAsAReferencedIssue(t *testing.T) {
	env := basicEnv()
	env["STUB_PRBODY"] = "Implements #12. See the size proof in #42 itself."

	r := runPacket(t, env, nil, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustNotContain(t, r.stdout, "Issue #42", "the PR must not fetch or inline itself")
	if r.called(`pr view 42 --json number`) || r.called(`issue view 42`) {
		t.Errorf("the PR's own number must be excluded before any lookup, but called:\n%s", r.calls)
	}
}

// A #N that resolves to another pull request (not this one) is noted, not
// fatal - checked with `gh pr view`, since `gh issue view` on a PR number
// succeeds and returns that PR's own body rather than failing.
func TestPacketReferencedNumberThatIsAPullRequestIsNotedNotFatal(t *testing.T) {
	env := basicEnv()
	env["STUB_PRBODY"] = "See #999 for the earlier attempt."
	env["STUB_PR_999_EXISTS"] = "1"

	r := runPacket(t, env, nil, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "#999", "the packet")
	mustContain(t, r.stdout, "not an issue", "the packet")
	if r.called("issue view 999") {
		t.Errorf("a number already confirmed to be a PR must not also be fetched as an issue:\n%s", r.calls)
	}
}

// A #N that is neither this PR nor another PR but still cannot be read as an
// issue (deleted, or a typo) is noted the same way, not fatal.
func TestPacketReferencedNumberThatCannotBeReadIsNotedNotFatal(t *testing.T) {
	env := basicEnv()
	env["STUB_PRBODY"] = "See #999 for the earlier attempt."
	env["STUB_ISSUE_999_FOUND"] = "0"

	r := runPacket(t, env, nil, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "#999", "the packet")
	mustContain(t, r.stdout, "not an issue", "the packet")
}

// A heading with a backtick-quoted term and a $-sigil, the shape several
// real specs use (e.g. "## `docker-compose.yml` (base/production)"). An
// unquoted heredoc expands backticks and $ in its body before a read loop
// ever sees it, which silently ate this heading and its section the first
// time this script fed headings through one; headings must reach the match
// loop through a plain file redirect instead.
const backtickSpec = "# 30 - Backtick Spec\n\n" +
	"## `docker-compose.yml` (base/production) and $HOME\n\n" +
	"Do not run `rm -rf $HOME` here.\n\n" +
	"## Unrelated\n\nFiller.\n"

func TestPacketSpecHeadingWithBackticksAndDollarSurvives(t *testing.T) {
	env := basicEnv()
	env["STUB_PRBODY"] = "Implements #12. See docs/specs/30-backtick-spec.md, " +
		"section \"`docker-compose.yml` (base/production) and $HOME\"."

	r := runPacket(t, env, map[string]string{"docs/specs/30-backtick-spec.md": backtickSpec}, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "docker-compose.yml` (base/production) and $HOME", "the packet")
	mustContain(t, r.stdout, "Do not run `rm -rf $HOME` here.", "the section body")
	mustNotContain(t, r.stdout, "Filler.", "the unmatched section")
}

// Prose wraps a heading's own words across lines - this project's own PR
// bodies do it constantly - so a heading is named even when the PR body's
// line break falls in the middle of it, as long as the words are in order.
func TestPacketSpecHeadingNamedAcrossALineWrapStillMatches(t *testing.T) {
	env := basicEnv()
	env["STUB_PRBODY"] = "Implements #12. See docs/specs/09-test-spec.md, section \"Widget Behavior\n" +
		"Rules\"."

	r := runPacket(t, env, map[string]string{"docs/specs/09-test-spec.md": testSpec}, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "Widgets must be blue.", "the wrapped heading still matches")
	mustNotContain(t, r.stdout, "Nothing to do with widgets.", "the unmatched section")
}

// --- spec sections ----------------------------------------------------------

const testSpec = `# 09 - Test Spec

## Overview

Background nobody named.

## Widget Behavior Rules

Widgets must be blue.

### A sub-rule

Only on Tuesdays.

## Unrelated Section

Nothing to do with widgets.
`

// TestPacketSpecSectionNamedHeadingOnly is acceptance criterion 1: naming one
// heading pulls that section (its sub-heading included) and leaves the
// unmatched sections out, with an omission note.
func TestPacketSpecSectionNamedHeadingOnly(t *testing.T) {
	env := basicEnv()
	env["STUB_PRBODY"] = "Implements #12. See docs/specs/09-test-spec.md, section \"Widget Behavior Rules\"."

	r := runPacket(t, env, map[string]string{"docs/specs/09-test-spec.md": testSpec}, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "Widget Behavior Rules", "the packet")
	mustContain(t, r.stdout, "Widgets must be blue.", "the packet")
	mustContain(t, r.stdout, "Only on Tuesdays.", "the sub-heading stays with its section")
	mustContain(t, r.stdout, "other headings omitted", "the omission note")
	mustNotContain(t, r.stdout, "Nothing to do with widgets.", "the unmatched section")
	mustNotContain(t, r.stdout, "Background nobody named.", "the unmatched section")
}

// A spec named in the PR or issue body but absent from this checkout (a
// rename, or a spec added by a package this PR hasn't merged with yet) must
// be noted, not fatal.
func TestPacketSpecNamedButNotFoundInThisCheckout(t *testing.T) {
	env := basicEnv()
	env["STUB_PRBODY"] = "Implements #12 (docs/specs/99-does-not-exist.md)."

	r := runPacket(t, env, nil, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "docs/specs/99-does-not-exist.md", "the packet")
	mustContain(t, r.stdout, "not found in this checkout", "the packet")
}

// A spec of 300 lines or fewer with no heading named is small enough to
// inline whole - the 300-line ceiling exists for the specs that are not.
func TestPacketSpecAtOrUnderThreeHundredLinesWithNoHeadingNamedIsInlinedWhole(t *testing.T) {
	env := basicEnv()
	env["STUB_PRBODY"] = "Implements #12 (docs/specs/09-test-spec.md)."

	r := runPacket(t, env, map[string]string{"docs/specs/09-test-spec.md": testSpec}, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "whole file", "the packet")
	mustContain(t, r.stdout, "Background nobody named.", "every section is present")
	mustContain(t, r.stdout, "Nothing to do with widgets.", "every section is present")
}

const longSpecPrefix = `# 20 - Long Spec

## Acceptance criteria

- [ ] It works.

## Filler
`

// TestPacketSpecFallsBackToAcceptanceCriteriaOverThreeHundredLines is
// acceptance criterion 1's other half: no heading named, and the file is too
// long to inline whole, so only Acceptance criteria plus the headings list
// survive, and the packet says the body was left out.
func TestPacketSpecFallsBackToAcceptanceCriteriaOverThreeHundredLines(t *testing.T) {
	var b strings.Builder
	b.WriteString(longSpecPrefix)
	for i := 0; i < 310; i++ {
		b.WriteString("Filler line.\n")
	}
	env := basicEnv()
	env["STUB_PRBODY"] = "Implements #12 (docs/specs/20-long-spec.md)."

	r := runPacket(t, env, map[string]string{"docs/specs/20-long-spec.md": b.String()}, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "Acceptance criteria", "the packet")
	mustContain(t, r.stdout, "It works.", "the packet")
	mustContain(t, r.stdout, "over 300", "the omission note")
	mustContain(t, r.stdout, "Filler", "the headings list still names Filler")
	mustNotContain(t, r.stdout, "Filler line.", "the body was left out, only the heading survives")
}

// --- exported identifiers ----------------------------------------------------

// TestPacketFlagsAFuncWithoutADocComment is acceptance criterion 2.
func TestPacketFlagsAFuncWithoutADocComment(t *testing.T) {
	env := basicEnv()
	env["STUB_DIFF_FULL"] = "diff --git a/internal/foo/foo.go b/internal/foo/foo.go\n" +
		"+++ b/internal/foo/foo.go\n" +
		"@@ -1,1 +1,10 @@\n" +
		" package foo\n" +
		"+\n" +
		"+// Bar does something useful.\n" +
		"+func Bar() error {\n" +
		"+\treturn nil\n" +
		"+}\n" +
		"+\n" +
		"+func Baz() error {\n" +
		"+\treturn nil\n" +
		"+}\n" +
		"+\n" +
		"+type Widget struct{}\n"

	r := runPacket(t, env, nil, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "| Bar | func | yes |", "a documented func is flagged yes")
	mustContain(t, r.stdout, "| Baz | func | no |", "an undocumented func is flagged no")
	mustContain(t, r.stdout, "| Widget | type | no |", "an undocumented type is flagged no")
}

// --- --since ------------------------------------------------------------------

// TestPacketSinceInlinesOnlyTheDeltaPlusWholeStat is acceptance criterion 3.
func TestPacketSinceInlinesOnlyTheDeltaPlusWholeStat(t *testing.T) {
	env := basicEnv()
	env["STUB_DIFF_FULL"] = "+func ShouldNotAppear() error { return nil }\n"
	env["STUB_DIFF_DELTA"] = "+func OnlyTheDelta() error { return nil }\n"

	r := runPacket(t, env, nil, "42", "--since", "priorSHA123")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "OnlyTheDelta", "the delta diff is inlined")
	mustContain(t, r.stdout, "internal/foo/foo.go | 7 +++++++", "the whole-PR stat is still included")
	mustNotContain(t, r.stdout, "ShouldNotAppear", "the full diff must not be inlined on a delta round")
	mustContain(t, r.stdout, "not inlined", "the packet points at the full diff instead of inlining it")
	if !r.called("git diff priorSHA123..") {
		t.Errorf("expected the two-dot delta range, got calls:\n%s", r.calls)
	}
}

// A merge commit between rounds turns the delta into upstream noise (D9), so
// the packet falls back to the full PR diff and says why.
func TestPacketSinceFallsBackToFullDiffOnAMergeCommit(t *testing.T) {
	env := basicEnv()
	env["STUB_MERGES"] = "abcdef1 Merge branch 'integration/harness-welle-2' into h7/review-packet"
	env["STUB_DIFF_FULL"] = "+func FromTheWholeDiff() error { return nil }\n"
	env["STUB_DIFF_DELTA"] = "+func ShouldNotAppear() error { return nil }\n"

	r := runPacket(t, env, nil, "42", "--since", "priorSHA123")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "FromTheWholeDiff", "the full diff is inlined on a merge")
	mustContain(t, r.stdout, "found a merge commit", "the packet explains the fallback")
	mustNotContain(t, r.stdout, "ShouldNotAppear", "the delta must not be used once a merge is found")
}

// --- previous verdict (round >= 2) -------------------------------------------

func TestPacketSinceFindsThePreviousVerdictByTheHeaderFormat(t *testing.T) {
	env := basicEnv()
	env["STUB_COMMENT_1"] = "## Go Review — VERDICT: BLOCK\n\n**Round:** 1  ·  **Spec:** docs/specs/01-architecture-and-deployment.md"
	env["STUB_COMMENT_2"] = "## Test Review — VERDICT: APPROVE\n\n**Round:** 1"
	env["STUB_COMMENT_3"] = "## Docs Review — VERDICT: APPROVE\n\n**Round:** 1"

	r := runPacket(t, env, nil, "42", "--since", "priorSHA123")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "Go Review — VERDICT: BLOCK", "the previous round's Go verdict")
	mustContain(t, r.stdout, "Test Review — VERDICT: APPROVE", "the previous round's test verdict")
	mustContain(t, r.stdout, "Docs Review — VERDICT: APPROVE", "the previous round's docs verdict")
	// The regression this test would have caught: join("") (no separator)
	// concatenates all three comments into one unsplit record, and only the
	// first one (whichever GitHub returns first) would ever be found. The
	// call log carries the query's literal source text (jq's own record-
	// separator escape), not a decoded byte - nothing here runs real jq.
	if !r.called(`join("\u001e")`) {
		t.Errorf("expected the real gh query to join with the \\u001e escape, got calls:\n%s", r.calls)
	}
}

// The H5 marker (docs/plans/2026-09-harness-optimization.md decision C3),
// once it exists in a comment, is preferred over the older header text - and
// only the latest of several rounds' markers is kept.
func TestPacketSinceFindsThePreviousVerdictByTheH5Marker(t *testing.T) {
	env := basicEnv()
	env["STUB_COMMENT_1"] = "## Go Review — VERDICT: BLOCK\n<!-- verdict: BLOCK round=1 sha=aaa reviewer=go -->"
	env["STUB_COMMENT_2"] = "## Go Review — VERDICT: APPROVE\n<!-- verdict: APPROVE round=2 sha=bbb reviewer=go -->"

	r := runPacket(t, env, nil, "42", "--since", "priorSHA123")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "round=2 sha=bbb reviewer=go", "the latest marker wins")
	mustNotContain(t, r.stdout, "round=1 sha=aaa", "the stale marker must not be shown")
}

func TestPacketRound1DoesNotLookForAPreviousVerdict(t *testing.T) {
	env := basicEnv()
	env["STUB_COMMENT_1"] = "## Go Review — VERDICT: APPROVE\n\n**Round:** 1"

	r := runPacket(t, env, nil, "42")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d: %s", r.exit, r.stderr)
	}
	if r.called("--json comments") {
		t.Errorf("round 1 has no previous round to look up, but comments were fetched:\n%s", r.calls)
	}
	mustNotContain(t, r.stdout, "Previous round", "round 1 has no previous-round section")
}
