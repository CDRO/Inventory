// Package scripts carries no application code: this directory holds the
// repository's tooling - the wave orchestrator, the backup script, and the
// `scripts/dev` dispatcher with its commands under scripts/dev.d/. The test
// lives here for the same reason deploy/synology/update_test.go lives next to
// `update`: `docker compose run --rm app go test ./...`
// (docs/specs/01-architecture-and-deployment.md) is the only test runner this
// project has, so a shell script is tested from Go, under the dev image's
// busybox sh.
//
// `scripts/dev` reaches the outside world through two commands, docker and
// gh. Every scenario below puts a recording stub for each of them first on
// PATH, runs the real dispatcher against a throwaway copy of the scripts
// directory, and asserts on what was printed, what exit code came back and
// what the stubs were asked to do. No daemon, no GitHub, no clone.
package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dockerStub answers `docker compose run --rm app go test|vet ...` with canned
// output and exit code, and records every call.
const dockerStub = `#!/bin/sh
printf 'docker %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  "compose run --rm app go test"*)
    [ -z "${STUB_TEST_OUTPUT:-}" ] || printf '%s\n' "$STUB_TEST_OUTPUT"
    exit ${STUB_TEST_RC:-0} ;;
  "compose run --rm app go vet"*)
    [ -z "${STUB_VET_OUTPUT:-}" ] || printf '%s\n' "$STUB_VET_OUTPUT"
    exit ${STUB_VET_RC:-0} ;;
esac
exit 0
`

// ghStub models the four gh calls the commands make. `run list` answers from a
// call counter so a scenario can make the run appear only on the n-th poll -
// the case the dispatch-and-poll recipe exists for. The --jq expressions the
// real gh would evaluate are ignored: the stub prints what they would have
// produced, one line per run ("<id>\t<name>") or per job
// ("<started>\t<completed>\t<labels>"). That is the one thing this file
// cannot prove: that those field projections name the right JSON keys. They
// carry no logic on purpose - every decision ci-usage makes (a job still
// running, a self-hosted job, the rounding, the projection) happens in awk
// on those lines, which the fixtures below reach.
const ghStub = `#!/bin/sh
printf 'gh %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  "workflow run "*)
    exit ${STUB_DISPATCH_RC:-0} ;;
  "run list "*)
    n=$(cat "$STUB_DIR/runlist" 2>/dev/null || echo 0)
    n=$((n + 1)); echo "$n" > "$STUB_DIR/runlist"
    if [ -n "${STUB_RUN_ID:-}" ] && [ "$n" -ge "${STUB_RUN_FOUND_AT:-1}" ]; then echo "$STUB_RUN_ID"; fi
    exit 0 ;;
  "run view "*)
    echo "https://github.com/CDRO/Inventory/actions/runs/${STUB_RUN_ID:-0}"; exit 0 ;;
  "run watch "*)
    exit ${STUB_WATCH_RC:-0} ;;
  "api --paginate "*"/actions/runs?"*)
    [ -z "${STUB_RUNS:-}" ] || printf '%s\n' "$STUB_RUNS"
    exit ${STUB_RUNS_RC:-0} ;;
  "api "*"/actions/runs/"*"/jobs"*)
    id=${2#*/actions/runs/}; id=${id%%/*}
    eval "out=\${STUB_JOBS_$id:-}"
    [ -z "$out" ] || printf '%s\n' "$out"
    exit 0 ;;
esac
exit 0
`

// The poll loop in ci-status sleeps between attempts and its own counter, not
// the clock, decides when it gives up - a sleep that returns at once costs no
// coverage and saves the suite the seconds. The Dockerfile's builder stage runs
// `go test ./...`, so every image build would otherwise wait them out too.
const sleepStub = `#!/bin/sh
exit 0
`

type result struct {
	exit   int
	stdout string
	stderr string
	calls  string // every stub invocation, in order, one per line
	root   string // the throwaway repository root the dispatcher ran in
}

func (r result) called(substr string) bool { return strings.Contains(r.calls, substr) }

// run copies scripts/dev and scripts/dev.d/* into a throwaway repository root
// (the commands locate the root as $(dirname $0)/../.., so the copy sits at
// that depth), writes the stubs, and runs the dispatcher with args.
func run(t *testing.T, env map[string]string, args ...string) result {
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
		"docker": dockerStub,
		"gh":     ghStub,
		"sleep":  sleepStub,
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	log := filepath.Join(root, "calls.log")
	vars := map[string]string{
		// Stubs first; the rest is the image's own PATH, because sed, grep,
		// tail, awk and printf have to be the real ones.
		"PATH":     stubs + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"STUB_LOG": log,
		"STUB_DIR": root,
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
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running scripts/dev: %v", err)
		}
		exit = ee.ExitCode()
	}

	calls, _ := os.ReadFile(log)
	r := result{exit: exit, stdout: stdout.String(), stderr: stderr.String(), calls: string(calls), root: root}
	t.Logf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s--- calls ---\n%s", r.exit, r.stdout, r.stderr, r.calls)
	return r
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: expected to find %q", what, needle)
	}
}

func mustNotContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: did not expect %q", what, needle)
	}
}

func lines(s string) int { return len(strings.Split(strings.TrimRight(s, "\n"), "\n")) }

// --- the dispatcher -------------------------------------------------------

// TestNoCommandListsEveryCommandAndExits2 pins the contract the issue states:
// no argument is a mistake (exit 2) that prints the command list, built from
// the second line of every file in dev.d - so the list can never omit a
// command that exists.
func TestNoCommandListsEveryCommandAndExits2(t *testing.T) {
	r := run(t, nil)
	if r.exit != 2 {
		t.Fatalf("expected exit 2, got %d", r.exit)
	}
	for _, c := range []string{"test", "vet", "ci-status", "ci-usage"} {
		mustContain(t, r.stdout, "\n  "+c, "usage lists "+c)
	}
	mustContain(t, r.stdout, "Run the Go suite in the dev container", "usage carries the test command's description")
	mustContain(t, r.stdout, "Wait for the GitHub Actions run of an exact commit", "usage carries the ci-status description")
	if r.calls != "" {
		t.Errorf("usage must not run docker or gh, but called:\n%s", r.calls)
	}
}

func TestUnknownCommandExits2AndNamesIt(t *testing.T) {
	r := run(t, nil, "frobnicate")
	if r.exit != 2 {
		t.Fatalf("expected exit 2, got %d", r.exit)
	}
	mustContain(t, r.stderr, "unknown command 'frobnicate'", "stderr")
	mustContain(t, r.stderr, "ci-usage", "the usage follows the error, on stderr")
}

// A command name with a slash or a leading dot could otherwise walk out of
// dev.d ("../../scripts/backup" is a file that exists).
func TestCommandNamesAreFileNamesOnly(t *testing.T) {
	for _, bad := range []string{"../dev", "dev.d/test", ".hidden"} {
		r := run(t, nil, bad)
		if r.exit != 2 {
			t.Errorf("%q: expected exit 2, got %d", bad, r.exit)
		}
		if r.calls != "" {
			t.Errorf("%q: must not run anything, but called:\n%s", bad, r.calls)
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	r := run(t, nil, "help")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.stdout, "commands (scripts/dev.d/):", "stdout")
}

// A command's --help is its header comment and nothing after it: a fixed line
// range would leak the first line of code (`set -u`) the moment the header
// grows or shrinks by a line.
func TestCommandHelpIsTheHeaderAndNothingElse(t *testing.T) {
	for _, c := range []string{"ci-status", "ci-usage"} {
		r := run(t, nil, c, "--help")
		if r.exit != 0 {
			t.Errorf("%s --help: expected exit 0, got %d", c, r.exit)
		}
		mustContain(t, r.stdout, "scripts/dev "+c, c+" --help names its own usage")
		mustNotContain(t, r.stdout, "set -u", c+" --help must not leak code")
		if r.calls != "" {
			t.Errorf("%s --help must not call gh, but called:\n%s", c, r.calls)
		}
	}
}

// --- test -----------------------------------------------------------------

const redLog = ` Container probe-db-1  Running
=== RUN   TestFoo
--- FAIL: TestFoo (0.00s)
    foo_test.go:12: expected 200, got 404
=== RUN   TestBar
--- PASS: TestBar (0.00s)
panic: boom [recovered]
	panic: boom
FAIL	github.com/CDRO/Inventory/internal/httpapi	0.512s
ok  	github.com/CDRO/Inventory/internal/store	1.204s
FAIL`

// TestTestCompactsARedSuite is the reason the command exists: the FAIL lines,
// the file:line under them and the panic reach the session; the noise does
// not; the exit code is the suite's; the whole log is still on disk.
func TestTestCompactsARedSuite(t *testing.T) {
	r := run(t, map[string]string{"STUB_TEST_OUTPUT": redLog, "STUB_TEST_RC": "1"}, "test")
	if r.exit != 1 {
		t.Fatalf("expected the suite's exit 1, got %d", r.exit)
	}
	if !r.called("docker compose run --rm app go test ./...") {
		t.Errorf("expected the documented command with ./..., got calls:\n%s", r.calls)
	}
	for _, want := range []string{"--- FAIL: TestFoo", "foo_test.go:12", "panic: boom", "FAIL\tgithub.com/CDRO/Inventory/internal/httpapi", "exit=1"} {
		mustContain(t, r.stdout, want, "stdout")
	}
	mustNotContain(t, r.stdout, "=== RUN", "stdout keeps the RUN lines out")
	mustNotContain(t, r.stdout, "--- PASS", "stdout keeps the PASS lines out")

	log, err := os.ReadFile(filepath.Join(r.root, ".claude", "last-test.log"))
	if err != nil {
		t.Fatalf("the full log must be on disk: %v", err)
	}
	if strings.TrimSpace(string(log)) != strings.TrimSpace(redLog) {
		t.Errorf("last-test.log must carry the complete output, got:\n%s", log)
	}
}

func TestTestGreenSuiteIsAHandfulOfLines(t *testing.T) {
	var b strings.Builder
	b.WriteString(" Container probe-db-1  Running\n")
	for _, p := range []string{"auth", "barcode", "config", "consume", "expiry", "gamification", "httpapi", "images", "imagesearch", "ingest", "jobs", "matching", "migrate", "notify", "store", "uploads", "vision"} {
		b.WriteString("ok  \tgithub.com/CDRO/Inventory/internal/" + p + "\t0.100s\n")
	}
	r := run(t, map[string]string{"STUB_TEST_OUTPUT": strings.TrimRight(b.String(), "\n")}, "test")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	if n := lines(r.stdout); n > 5 {
		t.Errorf("a green suite must cost at most 5 lines of stdout, got %d:\n%s", n, r.stdout)
	}
	mustContain(t, r.stdout, "exit=0", "stdout")
}

func TestTestPassesPackagesThrough(t *testing.T) {
	r := run(t, nil, "test", "./internal/store/", "./internal/migrate/")
	if !r.called("go test ./internal/store/ ./internal/migrate/") {
		t.Errorf("expected the packages to reach go test, got calls:\n%s", r.calls)
	}
	if r.called("./...") {
		t.Errorf("./... must not be added when packages are given:\n%s", r.calls)
	}
}

// --- vet ------------------------------------------------------------------

func TestVetKeepsFindingsAndDropsComposeNoise(t *testing.T) {
	out := " Container probe-db-1  Running\n Container probe-db-1  Healthy\n# github.com/CDRO/Inventory/internal/store\ninternal/store/x.go:10:2: unreachable code"
	r := run(t, map[string]string{"STUB_VET_OUTPUT": out, "STUB_VET_RC": "1"}, "vet")
	if r.exit != 1 {
		t.Fatalf("expected vet's exit 1, got %d", r.exit)
	}
	mustContain(t, r.stdout, "internal/store/x.go:10:2: unreachable code", "stdout")
	mustNotContain(t, r.stdout, "Container probe-db-1", "stdout drops Compose's status lines")
	mustContain(t, r.stdout, "exit=1", "stdout")
}

// --- ci-status ------------------------------------------------------------

// A full SHA, the shape `git rev-parse HEAD` prints and the only shape
// GitHub's `--commit` filter matches.
const fullSHA = "687184d0000000000000000000000000abcdef12"

func TestCiStatusFindsTheRunBySHAThenWatchesIt(t *testing.T) {
	r := run(t, map[string]string{"STUB_RUN_ID": "4242", "STUB_RUN_FOUND_AT": "3"}, "ci-status", fullSHA)
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	if n := strings.Count(r.calls, "gh run list --workflow test.yml --commit "+fullSHA); n != 3 {
		t.Errorf("expected the run to be looked up by exact commit until it appeared (3 polls), got %d:\n%s", n, r.calls)
	}
	if !r.called("gh run watch 4242 --exit-status") {
		t.Errorf("expected gh run watch on the found id, got calls:\n%s", r.calls)
	}
	mustContain(t, r.stdout, "https://github.com/CDRO/Inventory/actions/runs/4242", "stdout names the run URL")
	if r.called("workflow run") {
		t.Errorf("nothing may be dispatched without --dispatch:\n%s", r.calls)
	}
}

// An abbreviated SHA cannot go through `--commit` (GitHub matches the full
// SHA only, so `ci-status 687184d` would never find a run that exists); it is
// matched by prefix over the workflow's recent runs instead.
func TestCiStatusMatchesAnAbbreviatedSHAByPrefix(t *testing.T) {
	r := run(t, map[string]string{"STUB_RUN_ID": "77"}, "ci-status", "687184d")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	if r.called("--commit") {
		t.Errorf("an abbreviated SHA must not be passed to --commit:\n%s", r.calls)
	}
	if !r.called("gh run list --workflow test.yml --limit 50 --json databaseId,headSha") {
		t.Errorf("expected a prefix match over the recent runs, got calls:\n%s", r.calls)
	}
	if !r.called(`startswith("687184d")`) {
		t.Errorf("expected the prefix filter to carry the SHA, got calls:\n%s", r.calls)
	}
}

func TestCiStatusRejectsSomethingThatIsNotASHA(t *testing.T) {
	for _, bad := range []string{"main", "abc", "687184d;rm", fullSHA + "0"} {
		r := run(t, nil, "ci-status", bad)
		if r.exit != 2 {
			t.Errorf("%q: expected exit 2, got %d", bad, r.exit)
		}
		mustContain(t, r.stderr, "is not a commit SHA", bad+": stderr")
		if r.calls != "" {
			t.Errorf("%q: must not call gh, but called:\n%s", bad, r.calls)
		}
	}
}

func TestCiStatusAttemptsMustBeANumber(t *testing.T) {
	for _, bad := range []string{"many", "", "2x"} {
		r := run(t, nil, "ci-status", fullSHA, "--attempts", bad)
		if r.exit != 2 {
			t.Errorf("--attempts %q: expected exit 2, got %d", bad, r.exit)
		}
		mustContain(t, r.stderr, "--attempts wants a number", "stderr")
		if r.calls != "" {
			t.Errorf("--attempts %q: must not call gh, but called:\n%s", bad, r.calls)
		}
	}
}

func TestCiStatusExitsWithTheRunsResult(t *testing.T) {
	r := run(t, map[string]string{"STUB_RUN_ID": "7", "STUB_WATCH_RC": "1"}, "ci-status", fullSHA)
	if r.exit != 1 {
		t.Fatalf("a failed run must be a non-zero exit, got %d", r.exit)
	}
}

func TestCiStatusWithoutARunNamesDispatch(t *testing.T) {
	r := run(t, nil, "ci-status", fullSHA, "--attempts", "2")
	if r.exit != 3 {
		t.Fatalf("expected exit 3 for no run, got %d", r.exit)
	}
	mustContain(t, r.stderr, "--dispatch", "stderr names the flag that would start one")
	if n := strings.Count(r.calls, "gh run list"); n != 2 {
		t.Errorf("expected exactly --attempts polls, got %d", n)
	}
	if r.called("gh run watch") {
		t.Errorf("nothing to watch, yet watch was called:\n%s", r.calls)
	}
}

func TestCiStatusDispatchesFirstWhenAsked(t *testing.T) {
	r := run(t, map[string]string{"STUB_RUN_ID": "9"}, "ci-status", fullSHA, "--dispatch", "feature/x", "--workflow", "e2e.yml")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	if !r.called("gh workflow run e2e.yml --ref feature/x") {
		t.Errorf("expected the dispatch, got calls:\n%s", r.calls)
	}
	if !r.called("gh run list --workflow e2e.yml --commit " + fullSHA) {
		t.Errorf("expected the poll on the named workflow, got calls:\n%s", r.calls)
	}
	if strings.Index(r.calls, "workflow run") > strings.Index(r.calls, "run list") {
		t.Errorf("the dispatch must come before the first poll:\n%s", r.calls)
	}
}

func TestCiStatusUsageErrors(t *testing.T) {
	if r := run(t, nil, "ci-status"); r.exit != 2 {
		t.Errorf("no SHA: expected exit 2, got %d", r.exit)
	}
	if r := run(t, nil, "ci-status", fullSHA, "--bogus"); r.exit != 2 {
		t.Errorf("unknown option: expected exit 2, got %d", r.exit)
	}
}

// --- ci-usage -------------------------------------------------------------

// TestCiUsageBillsEachJobRoundedUp: a 70-second job bills 2 minutes, a
// 190-second one 4 and a 95-second one 2 - the rounding GitHub applies per
// job, which is what makes the plan's baseline 620 minutes for 310 runs of
// 1.4 minutes rather than 434.
func TestCiUsageBillsEachJobRoundedUp(t *testing.T) {
	env := map[string]string{
		"STUB_RUNS":   "1\ttest\n2\te2e\n3\ttest",
		"STUB_JOBS_1": "2026-08-01T10:00:00Z\t2026-08-01T10:01:10Z\tubuntu-latest",
		"STUB_JOBS_2": "2026-08-02T10:00:00Z\t2026-08-02T10:03:10Z\tubuntu-latest\n2026-08-02T10:00:00Z\t2026-08-02T10:01:35Z\tubuntu-latest",
		"STUB_JOBS_3": "2026-08-03T23:59:30Z\t2026-08-04T00:00:29Z\t",
	}
	r := run(t, env, "ci-usage", "--month", "2026-08")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	if !r.called("actions/runs?created=2026-08-01..2026-08-31&per_page=100") {
		t.Errorf("expected the month's inclusive date range, got calls:\n%s", r.calls)
	}
	for _, want := range []string{`(?m)^test\s+2\s+3$`, `(?m)^e2e\s+1\s+6$`, `(?m)^total\s+3\s+9$`, `31 of 31 days -> projected 9 of 2000 minutes`} {
		if !regexp.MustCompile(want).MatchString(r.stdout) {
			t.Errorf("stdout should match %s, got:\n%s", want, r.stdout)
		}
	}
}

// A job on a self-hosted runner is not billed, and a job that has not
// finished has no completed_at yet (jq prints "null"); both must be left out,
// or the NAS deploy job of the release pipeline would inflate the number.
func TestCiUsageSkipsSelfHostedAndUnfinishedJobs(t *testing.T) {
	env := map[string]string{
		"STUB_RUNS": "1\trelease\n2\ttest",
		// gate on a hosted runner (2 min), deploy on the NAS (must not count)
		"STUB_JOBS_1": "2026-08-05T10:00:00Z\t2026-08-05T10:01:30Z\tubuntu-latest\n2026-08-05T10:02:00Z\t2026-08-05T10:20:00Z\tself-hosted,nas,synology",
		// one finished job (1 min) and one still running
		"STUB_JOBS_2": "2026-08-06T10:00:00Z\t2026-08-06T10:00:50Z\tubuntu-latest\n2026-08-06T10:01:00Z\tnull\tubuntu-latest",
	}
	r := run(t, env, "ci-usage", "--month", "2026-08")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	for _, want := range []string{`(?m)^release\s+1\s+2$`, `(?m)^test\s+1\s+1$`, `(?m)^total\s+2\s+3$`} {
		if !regexp.MustCompile(want).MatchString(r.stdout) {
			t.Errorf("stdout should match %s, got:\n%s", want, r.stdout)
		}
	}
}

// The projection is the number the plan's cap is tracked against: for the
// current month it scales the billed total to the month's length by the days
// elapsed. DEV_TODAY pins "today" so the branch is reachable from a test.
func TestCiUsageProjectsTheCurrentMonth(t *testing.T) {
	env := map[string]string{
		"DEV_TODAY":   "2026-08-10",
		"STUB_RUNS":   "1\ttest",
		"STUB_JOBS_1": "2026-08-01T10:00:00Z\t2026-08-01T10:08:30Z\tubuntu-latest", // 9 min
	}
	// No --month: the month comes from DEV_TODAY.
	r := run(t, env, "ci-usage")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	if !r.called("actions/runs?created=2026-08-01..2026-08-31") {
		t.Errorf("expected the month to come from DEV_TODAY, got calls:\n%s", r.calls)
	}
	// 9 minutes in 10 of 31 days -> 9 * 31 / 10 = 27.9 -> 28
	mustContain(t, r.stdout, "month 2026-08: 10 of 31 days -> projected 28 of 2000 minutes (1%)", "stdout")

	// On the last day the projection is the total itself.
	env["DEV_TODAY"] = "2026-08-31"
	r = run(t, env, "ci-usage")
	mustContain(t, r.stdout, "31 of 31 days -> projected 9 of 2000 minutes", "stdout")
}

func TestCiUsageSaysSoWhenTheMonthIsEmpty(t *testing.T) {
	r := run(t, nil, "ci-usage", "--month", "2025-02")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.stdout, "no workflow runs in 2025-02", "stdout")
	if r.called("/jobs") {
		t.Errorf("no runs, so no job lookups:\n%s", r.calls)
	}
}

func TestCiUsageRejectsAMalformedMonth(t *testing.T) {
	for _, bad := range []string{"2026-9", "09-2026", "2026-13"} {
		if r := run(t, nil, "ci-usage", "--month", bad); r.exit != 2 {
			t.Errorf("%q: expected exit 2, got %d", bad, r.exit)
		}
	}
}
