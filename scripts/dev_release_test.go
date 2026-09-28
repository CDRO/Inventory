// scripts/dev.d/release is the operator half of the release pipeline
// (docs/specs/38-release-pipeline-and-nas-runner.md, H17): it decides whether
// a commit may be tagged at all, and everything it decides it decides before
// the tag exists. That is what this file covers.
//
// Its own harness rather than dev_test.go's `run`, for the reason
// dev_packet_test.go has one: `release` is the only command that shells out to
// git, so the stub set is different, and a command that both CREATES and
// PUSHES something needs its stubs to record more than a call log - the tag
// message in particular, which is where the classic override lives.
//
// No daemon, no clone, no GitHub: git and gh are recording stubs, and
// `scripts/dev.d/ci-status` - which `release` reuses rather than copying - runs
// for real against the gh stub, so a change to ci-status's exit codes shows up
// here as a broken refusal rather than as a passing test of a copy.
package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The commit every scenario is "on" unless it says otherwise. Forty hex
// characters on purpose: ci-status takes the `gh run list --commit` path only
// for a full SHA, and that is the path a real release always takes.
const releaseHead = "1111111111111111111111111111111111111111"

// releaseGitStub answers the nine git calls `release` makes and records them.
//
// It logs the first three arguments rather than "$*": the tag message is one
// of the arguments and it is deliberately multi-line, which would turn the
// call log into something no `strings.Contains` could read. The message is
// recorded on its own, in $STUB_DIR/tag-message, which is what the classic
// tests assert against.
const releaseGitStub = `#!/bin/sh
printf 'git %s %s %s\n' "${1:-}" "${2:-}" "${3:-}" >> "$STUB_LOG"
case "$*" in
  "rev-parse --abbrev-ref HEAD")
    printf '%s\n' "${STUB_BRANCH:-main}"; exit 0 ;;
  "status --porcelain")
    [ -z "${STUB_DIRTY:-}" ] || printf '%s\n' "$STUB_DIRTY"
    exit 0 ;;
  "fetch "*)
    exit ${STUB_FETCH_RC:-0} ;;
  "rev-parse HEAD")
    printf '%s\n' "${STUB_HEAD:-1111111111111111111111111111111111111111}"; exit 0 ;;
  "rev-parse origin/main")
    printf '%s\n' "${STUB_ORIGIN_MAIN:-${STUB_HEAD:-1111111111111111111111111111111111111111}}"; exit 0 ;;
  "rev-parse -q --verify refs/tags/"*)
    [ -n "${STUB_TAG_EXISTS:-}" ] || exit 1
    printf '%s\n' "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"; exit 0 ;;
  "tag -a "*)
    printf '%s' "$5" > "$STUB_DIR/tag-message"
    exit ${STUB_TAG_RC:-0} ;;
  "tag -d "*)
    exit 0 ;;
  "push origin "*)
    exit ${STUB_PUSH_RC:-0} ;;
esac
exit 0
`

// releaseGhStub serves both consumers: ci-status (run list by commit, run
// view, run watch) and release's own poll for the release run's URL. The run
// ids are the stub's routing keys - STUB_WATCH_<id> is that run's conclusion -
// so a scenario can make `test` green and `e2e` red without the stub knowing
// which workflow is which.
const releaseGhStub = `#!/bin/sh
printf 'gh %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  "run list --workflow release.yml"*)
    [ -z "${STUB_RELEASE_URL:-}" ] || printf '%s\n' "$STUB_RELEASE_URL"
    exit 0 ;;
  "run list --workflow test.yml"*)
    [ -z "${STUB_TEST_RUN:-}" ] || printf '%s\n' "$STUB_TEST_RUN"
    exit 0 ;;
  "run list --workflow e2e.yml"*)
    [ -z "${STUB_E2E_RUN:-}" ] || printf '%s\n' "$STUB_E2E_RUN"
    exit 0 ;;
  "run view "*)
    printf 'https://github.com/CDRO/Inventory/actions/runs/%s\n' "$3"; exit 0 ;;
  "run watch "*)
    eval "rc=\${STUB_WATCH_$3:-0}"
    exit "$rc" ;;
esac
exit 0
`

// runRelease copies the dispatcher and every command beside it into a
// throwaway root (all of dev.d, because `release` execs its sibling
// ci-status), writes the stubs, and runs `scripts/dev release …`.
func runRelease(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()

	root := t.TempDir()
	dst := filepath.Join(root, "scripts")
	if err := os.MkdirAll(filepath.Join(dst, "dev.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, "dev", filepath.Join(dst, "dev"))
	entries, err := os.ReadDir("dev.d")
	if err != nil {
		t.Fatalf("read dev.d: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		copyFile(t, filepath.Join("dev.d", e.Name()), filepath.Join(dst, "dev.d", e.Name()))
	}

	stubs := filepath.Join(root, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"git":   releaseGitStub,
		"gh":    releaseGhStub,
		"sleep": sleepStub,
	} {
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

	cmd := exec.Command("/bin/sh", append([]string{filepath.Join(dst, "dev"), "release"}, args...)...)
	cmd.Env = make([]string, 0, len(vars))
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running scripts/dev release: %v", err)
		}
		exit = ee.ExitCode()
	}

	calls, _ := os.ReadFile(log)
	r := result{exit: exit, stdout: stdout.String(), stderr: stderr.String(), calls: string(calls), root: root}
	t.Logf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s--- calls ---\n%s", r.exit, r.stdout, r.stderr, r.calls)
	return r
}

// green is the environment of a commit that may be released: on main, clean,
// identical to origin/main, with a successful run of both workflows.
func green() map[string]string {
	return map[string]string{
		"STUB_HEAD":        releaseHead,
		"STUB_TEST_RUN":    "111",
		"STUB_E2E_RUN":     "222",
		"STUB_RELEASE_URL": "https://github.com/CDRO/Inventory/actions/runs/999",
		"STUB_WATCH_111":   "0",
		"STUB_WATCH_222":   "0",
	}
}

func withEnv(base map[string]string, kv ...string) map[string]string {
	out := make(map[string]string, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func tagMessage(t *testing.T, r result) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.root, "tag-message"))
	if err != nil {
		t.Fatalf("no tag was created (no tag-message recorded): %v", err)
	}
	return string(b)
}

// mustNotHaveTagged is the assertion every refusal shares: a refusal that
// still created or pushed a tag would be worse than no check at all, because
// the tag is what the pipeline reacts to.
func mustNotHaveTagged(t *testing.T, r result) {
	t.Helper()
	if r.called("git tag -a") {
		t.Errorf("a refusal must not create the tag; calls were:\n%s", r.calls)
	}
	if r.called("git push origin") {
		t.Errorf("a refusal must not push anything; calls were:\n%s", r.calls)
	}
}

// --- the tag name ---------------------------------------------------------

// Spec 38: the release unit is vYYYY.MM.DD, or vYYYY.MM.DD.n for the n-th
// release of one day. The workflow triggers on `v*`, so a name this command
// let through would still start a release - it just would not be one anybody
// could read a date off.
func TestReleaseRefusesANameThatIsNotAReleaseTag(t *testing.T) {
	for _, bad := range []string{
		"v1.4.0",          // semantic versions are not what this project tags
		"2026.09.30",      // no leading v
		"v2026.9.30",      // month not zero-padded
		"v2026.09.30.",    // trailing dot, no counter
		"v2026.09.30.x",   // counter is not a number
		"v2026.09.30.1.2", // one counter, not a fourth level
		"v2026.09.30-rc1", // no pre-release suffixes
		"vYYYY.MM.DD",
	} {
		r := runRelease(t, green(), bad)
		if r.exit != 1 {
			t.Errorf("%q: expected exit 1, got %d", bad, r.exit)
		}
		mustContain(t, r.stderr, "is not a release tag name", bad+": stderr")
		mustNotHaveTagged(t, r)
	}
}

// The name is checked before anything else, so a typo costs no fetch and no
// API call - which is also what makes the refusal instant.
func TestReleaseChecksTheNameBeforeItTouchesGitOrGitHub(t *testing.T) {
	r := runRelease(t, green(), "v1.4.0")
	if r.calls != "" {
		t.Errorf("a bad tag name must be refused before any git or gh call, but called:\n%s", r.calls)
	}
}

func TestReleaseAcceptsTheSameDayCounter(t *testing.T) {
	r := runRelease(t, green(), "v2026.09.30.2")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", r.exit, r.stderr)
	}
	mustContain(t, r.calls, "git tag -a v2026.09.30.2", "the tag was created")
}

// --- a clean, fetched main ------------------------------------------------

func TestReleaseRefusesOffMain(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_BRANCH", "harness/h17-release-workflow"), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "a release is cut from main", "stderr")
	mustContain(t, r.stderr, "harness/h17-release-workflow", "stderr names the branch it found")
	mustNotHaveTagged(t, r)
}

func TestReleaseRefusesADirtyTree(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_DIRTY", " M internal/httpapi/router.go"), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "the clone is not clean", "stderr")
	mustContain(t, r.stderr, "internal/httpapi/router.go", "stderr shows what is dirty")
	mustNotHaveTagged(t, r)
}

// A dirty tree is refused before the fetch: the fetch is the slow step, and
// nothing about it could make an uncommitted change releasable.
func TestReleaseRefusesADirtyTreeBeforeFetching(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_DIRTY", "?? notes.txt"), "v2026.09.30")
	if r.called("git fetch") {
		t.Errorf("expected no fetch after a dirty-tree refusal; calls were:\n%s", r.calls)
	}
}

func TestReleaseRefusesAFailedFetch(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_FETCH_RC", "1"), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "git fetch origin failed", "stderr")
	mustNotHaveTagged(t, r)
}

// A commit GitHub has never seen has no runs to be green, and the tag push
// would carry it along silently.
func TestReleaseRefusesAHeadThatIsNotOriginMain(t *testing.T) {
	r := runRelease(t, withEnv(green(),
		"STUB_ORIGIN_MAIN", "2222222222222222222222222222222222222222"), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "is not origin/main", "stderr")
	mustNotHaveTagged(t, r)
}

func TestReleaseRefusesATagThatAlreadyExists(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_TAG_EXISTS", "1"), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "already exists", "stderr")
	mustNotHaveTagged(t, r)
}

// --- green runs for exactly this commit -----------------------------------

// ci-status exits 3 when no run for the SHA exists at all, which is a
// different situation from a red one and gets its own message - the one that
// tells the operator how to start a run.
func TestReleaseRefusesACommitWithNoRun(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_TEST_RUN", ""), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "no test.yml run for "+releaseHead, "stderr")
	mustContain(t, r.stderr, "--dispatch main", "stderr says how to get one")
	mustNotHaveTagged(t, r)
}

func TestReleaseRefusesARedRun(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_WATCH_111", "1"), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "the test.yml run for "+releaseHead+" did not succeed", "stderr")
	mustNotHaveTagged(t, r)
}

// Both workflows, not just the first one: spec 38's gate is `test` AND `e2e`,
// and a green `test` beside a missing `e2e` is exactly the case a single
// lookup would wave through.
func TestReleaseRequiresE2ETooNotOnlyTest(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_E2E_RUN", ""), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "no e2e.yml run for "+releaseHead, "stderr")
	mustNotHaveTagged(t, r)
}

func TestReleaseRefusesARedE2ERun(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_WATCH_222", "1"), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "the e2e.yml run for "+releaseHead+" did not succeed", "stderr")
	mustNotHaveTagged(t, r)
}

// The lookup is by the exact commit, never "the newest run on main" - the
// trap test.yml's own header documents and the reason ci-status exists.
func TestReleaseLooksTheRunsUpByTheCommit(t *testing.T) {
	r := runRelease(t, green(), "v2026.09.30")
	mustContain(t, r.calls, "--workflow test.yml --commit "+releaseHead, "the test lookup is by SHA")
	mustContain(t, r.calls, "--workflow e2e.yml --commit "+releaseHead, "the e2e lookup is by SHA")
}

// --- the tag it creates ---------------------------------------------------

func TestReleaseTagsPushesAndNamesTheRun(t *testing.T) {
	r := runRelease(t, green(), "v2026.09.30")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", r.exit, r.stderr)
	}
	mustContain(t, r.calls, "git tag -a v2026.09.30", "the annotated tag is created")
	mustContain(t, r.calls, "git push origin refs/tags/v2026.09.30", "the tag is pushed by its full ref")
	if msg := tagMessage(t, r); msg != "Release v2026.09.30" {
		t.Errorf("default tag message: got %q", msg)
	}
	mustContain(t, r.stdout, "pushed v2026.09.30 -> "+releaseHead, "stdout names the commit released")
	mustContain(t, r.stdout, "https://github.com/CDRO/Inventory/actions/runs/999", "stdout names the release run")
}

// Annotated, not lightweight: the tag object's message is the only place a
// forced-classic deploy can be declared, so a `git tag` without -a would make
// --classic silently impossible.
func TestReleaseCreatesAnAnnotatedTag(t *testing.T) {
	r := runRelease(t, green(), "v2026.09.30")
	mustContain(t, r.calls, "git tag -a", "the tag is annotated")
}

// Decision D3: the override is the exact line `deploy: classic` in the tag
// message. release.yml matches it with `grep -qx` after stripping carriage
// returns, so a line with anything else on it is not the keyword.
func TestReleaseClassicPutsTheKeywordOnItsOwnLine(t *testing.T) {
	r := runRelease(t, green(), "v2026.09.30", "--classic")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", r.exit, r.stderr)
	}
	msg := tagMessage(t, r)
	var found bool
	for _, line := range strings.Split(msg, "\n") {
		if line == "deploy: classic" {
			found = true
		}
	}
	if !found {
		t.Errorf("--classic must put the exact line %q into the message; got:\n%s", "deploy: classic", msg)
	}
	mustContain(t, msg, "Release v2026.09.30", "the default summary survives the keyword")
}

// Nothing forces rolling, and nothing accidentally forces classic either: a
// plain release must not carry the keyword.
func TestReleaseWithoutClassicCarriesNoKeyword(t *testing.T) {
	r := runRelease(t, green(), "v2026.09.30")
	mustNotContain(t, tagMessage(t, r), "deploy: classic", "a plain release's tag message")
}

func TestReleaseKeepsAGivenMessage(t *testing.T) {
	r := runRelease(t, green(), "v2026.09.30", "-m", "Shopping list and expiry cascade")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", r.exit, r.stderr)
	}
	if msg := tagMessage(t, r); msg != "Shopping list and expiry cascade" {
		t.Errorf("tag message: got %q", msg)
	}
}

// A message that already declares the override does not get a second copy:
// two keyword lines would not be a stronger override, only noise in the
// release notes.
func TestReleaseDoesNotRepeatAKeywordTheMessageAlreadyHas(t *testing.T) {
	r := runRelease(t, green(), "v2026.09.30", "--classic", "-m", "Drops products.legacy_id\n\ndeploy: classic")
	msg := tagMessage(t, r)
	if n := strings.Count(msg, "deploy: classic"); n != 1 {
		t.Errorf("expected exactly one keyword line, got %d:\n%s", n, msg)
	}
}

// A push that fails leaves the clone as it was, so the fix is to run the same
// command again rather than to remember to delete a local tag first.
func TestReleaseRemovesTheLocalTagWhenThePushFails(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_PUSH_RC", "1"), "v2026.09.30")
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.calls, "git tag -d v2026.09.30", "the local tag is removed again")
	mustContain(t, r.stderr, "could not push", "stderr")
}

// The tag is pushed; not finding its run yet is a note, not a failure. An
// operator who sees a non-zero exit here would reasonably conclude the release
// did not start, and would push the tag again.
func TestReleaseStillSucceedsWhenTheRunHasNotAppearedYet(t *testing.T) {
	r := runRelease(t, withEnv(green(), "STUB_RELEASE_URL", ""), "v2026.09.30")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "has not appeared yet", "stdout")
}

// --- the command line -----------------------------------------------------

func TestReleaseUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "usage: scripts/dev release"},
		{[]string{"v2026.09.30", "v2026.10.01"}, "one tag only"},
		{[]string{"v2026.09.30", "--frobnicate"}, "unknown option"},
		{[]string{"v2026.09.30", "-m"}, "needs a message"},
		{[]string{"v2026.09.30", "--attempts"}, "wants a number"},
		{[]string{"v2026.09.30", "--attempts", "soon"}, "wants a number"},
		{[]string{"v2026.09.30", "--attempts", "0"}, "at least 1"},
	} {
		r := runRelease(t, green(), tc.args...)
		if r.exit != 2 {
			t.Errorf("%v: expected exit 2, got %d", tc.args, r.exit)
		}
		mustContain(t, r.stderr, tc.want, strings.Join(tc.args, " ")+": stderr")
		mustNotHaveTagged(t, r)
	}
}

// Every command's --help is its header block and nothing after it; a fixed
// line range would leak the first line of code the moment the header grows.
func TestReleaseHelpIsTheHeaderAndNothingElse(t *testing.T) {
	r := runRelease(t, green(), "--help")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.stdout, "scripts/dev release <vYYYY.MM.DD[.n]>", "--help names its own usage")
	mustNotContain(t, r.stdout, "set -u", "--help must not leak code")
	if r.calls != "" {
		t.Errorf("--help must not call git or gh, but called:\n%s", r.calls)
	}
}

// The dispatcher builds its command list from the second line of every file in
// dev.d, so a command that exists is always listed - this pins that `release`
// is one of them and that its description line is the one line it should be.
func TestReleaseIsListedByTheDispatcher(t *testing.T) {
	r := run(t, nil)
	mustContain(t, r.stdout, "\n  release", "the usage lists release")
	mustContain(t, r.stdout, "Cut a release: tag a green commit on main", "the usage carries release's description")
}
