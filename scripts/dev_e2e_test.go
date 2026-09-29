// Tests for scripts/dev.d/e2e, in their own file with their own stub and
// runner (runE2E, not dev_test.go's run()) for the reason dev_packet_test.go
// gives: a second command's docker call shapes have nothing to do with the
// first's, and a shared stub would grow one case block every package's PR
// touches.
//
// The stub here is stateful about one thing, deliberately: a successful
// `docker network create` of the lock is remembered, so a later `network
// inspect` reports the lock as held by whoever claimed it and `network rm`
// clears it. That is what makes the lock's whole lifecycle — claim, refuse a
// second claim, reclaim your own, release on the way out — testable without a
// daemon, and the lifecycle is the point of the command (#389).
//
// mustContain, mustNotContain, copyFile and the result type come from
// dev_test.go.
package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// e2eDockerStub answers every docker call scripts/dev.d/e2e makes and records
// them all. Knobs, all optional:
//
//	STUB_LOCK_HELD=1   a foreign lock already exists
//	STUB_LOCK_DIR      that lock's working_dir label
//	STUB_LOCK_AGE      its age in seconds (claimed_epoch is derived from it)
//	STUB_STACK         `docker ps -a` rows for the project
//	STUB_RUNNER        rows for the `e2e` service (non-empty = a live suite)
//	STUB_APP           rows for the `app` service
//	STUB_APP_RECENT=1  `docker logs --since` finds something
//	STUB_SERVED        the asset self-check's `path digest` lines
//	STUB_SUITE_OUT     the suite's output
//	STUB_SUITE_RC      the suite's exit code
//	STUB_FAIL_STEP     a compose subcommand to fail (e.g. "build")
//	STUB_HEALTHZ_RC    the readiness poll's exit code
//	STUB_REFUSE_CREATE=1  the daemon refuses every `network create`, with no
//	                      lock existing — a dead daemon or an exhausted pool
const e2eDockerStub = `#!/bin/sh
printf 'docker %s\n' "$*" >> "$STUB_LOG"
lockfile="$STUB_DIR/lock"

case "$1 $2" in
  "network create")
    if [ "${STUB_REFUSE_CREATE:-0}" = "1" ]; then
      echo "Error response from daemon: all predefined address pools have been fully subnetted" >&2
      exit 1
    fi
    if [ -f "$lockfile" ] || [ "${STUB_LOCK_HELD:-0}" = "1" ]; then
      echo "Error response from daemon: network with name $3 already exists" >&2
      exit 1
    fi
    for a in "$@"; do
      case "$a" in
        inventory.e2e.lock.working_dir=*) printf '%s\n' "${a#*=}" > "$lockfile" ;;
      esac
    done
    echo 0123456789abcdef
    exit 0 ;;
  "network inspect")
    if [ -f "$lockfile" ]; then
      dir=$(sed -n 1p "$lockfile")
      age=$(sed -n 2p "$lockfile")
      [ -n "$age" ] || age=0
    elif [ "${STUB_LOCK_HELD:-0}" = "1" ]; then
      dir=${STUB_LOCK_DIR:-}; age=${STUB_LOCK_AGE:-60}
    else
      echo "Error response from daemon: network $3 not found" >&2
      exit 1
    fi
    case "$*" in
      *working_dir*)    printf '%s\n' "$dir" ;;
      *claimed_epoch*)  echo $(( $(date -u +%s) - age )) ;;
      *claimed_iso*)    echo 2026-09-29T09:00:00Z ;;
      *)                echo '[]' ;;
    esac
    exit 0 ;;
  "network rm")
    rm -f "$lockfile"
    exit 0 ;;
  "ps -a")
    case "$*" in
      *"service=e2e"*) [ -z "${STUB_RUNNER:-}" ] || printf '%s\n' "$STUB_RUNNER" ;;
      *"service=app"*) [ -z "${STUB_APP:-}" ] || printf '%s\n' "$STUB_APP" ;;
      *)               [ -z "${STUB_STACK:-}" ] || printf '%s\n' "$STUB_STACK" ;;
    esac
    exit 0 ;;
  "logs --since")
    [ "${STUB_APP_RECENT:-0}" = "1" ] && echo "2026-09-29T09:19:00Z GET /api/auth/me 200"
    exit 0 ;;
  "logs --timestamps")
    echo "${STUB_APP_LAST:-2026-09-29T08:50:15Z} GET /api/auth/me 200"
    exit 0 ;;
esac

# Everything else is a compose call. A step named in STUB_FAIL_STEP fails.
if [ -n "${STUB_FAIL_STEP:-}" ]; then
  case "$*" in
    *"${STUB_FAIL_STEP}"*)
      echo "compose: ${STUB_FAIL_STEP} exploded" >&2
      exit 17 ;;
  esac
fi

case "$*" in
  *"--entrypoint sh e2e -c "*)
    case "$*" in
      *healthz*)   exit ${STUB_HEALTHZ_RC:-0} ;;
      *sha256sum*) [ -z "${STUB_SERVED:-}" ] || printf '%s\n' "$STUB_SERVED"; exit 0 ;;
    esac
    exit 0 ;;
  *"-e E2E_WORKERS="*)
    [ -z "${STUB_SUITE_OUT:-}" ] || printf '%s\n' "$STUB_SUITE_OUT"
    exit ${STUB_SUITE_RC:-0} ;;
esac
exit 0
`

// The digests in the manifest the runner writes, and the served lines that
// match them. Two paths are enough: the check is a set comparison, and a third
// would only make the expected failure output longer.
const (
	manifestDigestA = "1111111111111111111111111111111111111111111111111111111111111111"
	manifestDigestB = "2222222222222222222222222222222222222222222222222222222222222222"
	foreignDigestB  = "3333333333333333333333333333333333333333333333333333333333333333"
)

const e2eManifest = `{
  "cache_version": "v36",
  "digests": {
    "/": "9999999999999999999999999999999999999999999999999999999999999999",
    "/css/base.css": "8888888888888888888888888888888888888888888888888888888888888888",
    "/js/api.js": "` + manifestDigestA + `",
    "/js/dom.js": "` + manifestDigestB + `"
  }
}
`

func servedOK() string {
	return "/js/api.js " + manifestDigestA + "\n/js/dom.js " + manifestDigestB
}

const suiteGreen = `Running 192 tests using 4 workers
  ✓  1 specs/app-shell.spec.js:12:3 › the shell renders (1.2s)
  ✓  2 specs/pwa.spec.js:30:3 › the worker installs (2.0s)
  192 passed (45.6s)
`

const suiteFlaky = `Running 192 tests using 4 workers
  ✓  1 specs/app-shell.spec.js:12:3 › the shell renders (1.2s)
  1) specs/products.spec.js:690:3 › a product saves › #status shows Saved.
     Error: expect(locator).toContainText(expected)
  1 failed
  2 flaky
  189 passed (51.3s)
`

// runE2E copies the dispatcher and its commands into a throwaway repository
// root, writes the docker stub and a web/shell-manifest.json, and runs
// `scripts/dev e2e <args>` there.
func runE2E(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()

	root := t.TempDir()
	dst := filepath.Join(root, "scripts")
	if err := os.MkdirAll(filepath.Join(dst, "dev.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, "dev", filepath.Join(dst, "dev"))
	copyFile(t, filepath.Join("dev.d", "e2e"), filepath.Join(dst, "dev.d", "e2e"))

	manifest := e2eManifest
	if m, ok := env["TEST_MANIFEST"]; ok {
		manifest = m
	}
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "shell-manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	stubs := filepath.Join(root, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubs, "docker"), []byte(e2eDockerStub), 0o755); err != nil {
		t.Fatal(err)
	}

	// TEST_PRESEED_LOCK writes the stub's own lock file before the run, so the
	// script finds a lock already held BY ITSELF — STUB_DIR is the repository
	// root the script resolves. The second line is the age the stub reports for
	// it, which is what decides ACTIVE versus STALE.
	if age, ok := map[string]string{"self-stale": "3000", "self-active": "45"}[env["TEST_PRESEED_LOCK"]]; ok {
		if err := os.WriteFile(filepath.Join(root, "lock"), []byte(root+"\n"+age+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else if env["TEST_PRESEED_LOCK"] != "" {
		t.Fatalf("unknown TEST_PRESEED_LOCK value %q", env["TEST_PRESEED_LOCK"])
	}

	log := filepath.Join(root, "calls.log")
	vars := map[string]string{
		"PATH":     stubs + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"STUB_LOG": log,
		"STUB_DIR": root,
	}
	for k, v := range env {
		if k == "TEST_MANIFEST" || k == "TEST_PRESEED_LOCK" {
			continue
		}
		vars[k] = v
	}

	cmd := exec.Command("/bin/sh", append([]string{filepath.Join(dst, "dev"), "e2e"}, args...)...)
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
			t.Fatalf("running scripts/dev e2e: %v", err)
		}
		exit = ee.ExitCode()
	}

	calls, _ := os.ReadFile(log)
	r := result{exit: exit, stdout: stdout.String(), stderr: stderr.String(), calls: string(calls), root: root}
	t.Logf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s--- calls ---\n%s", r.exit, r.stdout, r.stderr, r.calls)
	return r
}

// greenRun is the environment of a gate that is free and a suite that passes.
func greenRun() map[string]string {
	return map[string]string{
		"STUB_SERVED":    servedOK(),
		"STUB_SUITE_OUT": suiteGreen,
	}
}

// foreignActive is a lock held by another checkout with a suite really running:
// a container for the `e2e` service exists, which is the discriminator that
// settles it on its own (#377).
func foreignActive() map[string]string {
	return map[string]string{
		"STUB_LOCK_HELD":  "1",
		"STUB_LOCK_DIR":   "/c/Users/tizia/Projekte/Inventory-somewhere-else",
		"STUB_LOCK_AGE":   "120",
		"STUB_RUNNER":     "inventory-e2e-e2e-run-9f3 (Up 2 minutes)",
		"STUB_APP":        "inventory-e2e-app-1",
		"STUB_APP_RECENT": "1",
		"STUB_STACK":      "inventory-e2e-app-1  Up 3 minutes  C:\\Users\\tizia\\Projekte\\Inventory-somewhere-else",
	}
}

// foreignStale is the #377 shape exactly: a finished session's stack, still
// `Up` and `healthy`, with no runner container, nothing logged for half an
// hour, and a lock old enough that none of it is merely a quiet moment.
func foreignStale() map[string]string {
	return map[string]string{
		"STUB_LOCK_HELD": "1",
		"STUB_LOCK_DIR":  "/c/Users/tizia/Projekte/Inventory-somewhere-else",
		"STUB_LOCK_AGE":  "3000",
		"STUB_APP":       "inventory-e2e-app-1",
		"STUB_STACK":     "inventory-e2e-app-1  Up 50 minutes (healthy)  C:\\Users\\tizia\\Projekte\\Inventory-somewhere-else",
	}
}

// --- the command itself ----------------------------------------------------

func TestE2EIsListedByTheDispatcher(t *testing.T) {
	r := run(t, nil)
	mustContain(t, r.stdout, "\n  e2e", "usage lists the e2e command")
	mustContain(t, r.stdout, "Run the E2E deployment gate under a machine-wide lock",
		"usage carries the e2e command's description")
}

// Pinned as source text rather than behaviour because the failure it prevents
// only exists under Git Bash, and the Go suite runs in a Linux container where
// MSYS does not exist — so no amount of stubbing here can reproduce it.
//
// Measured on Git Bash before this line existed: the lock's own working_dir
// label came back as `C:/Users/...` where the script had written
// `/c/Users/...`, so the checkout did not recognise its own lock and never
// released it — the stack came down and the gate stayed locked, which is the
// exact failure #377 is about, reintroduced by the fix for it. The asset
// check's `/js/*.js` were rewritten the same way, so every fetch missed and a
// correct frontend was reported as foreign.
func TestE2EDisablesMSYSPathConversionForEveryContainerPathItPasses(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("dev.d", "e2e"))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(src), "export MSYS_NO_PATHCONV",
		"scripts/dev.d/e2e must disable MSYS path conversion for the whole script")
}

func TestE2EHelpIsTheHeaderAndRunsNothing(t *testing.T) {
	r := runE2E(t, nil, "--help")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.stdout, "scripts/dev e2e", "--help names its own usage")
	mustContain(t, r.stdout, "break-lock", "--help documents how a stale lock is broken")
	mustNotContain(t, r.stdout, "set -u", "--help must not leak code")
	if r.calls != "" {
		t.Errorf("--help must not run docker, but called:\n%s", r.calls)
	}
}

func TestE2ERejectsAnUnknownOptionAndANonNumericWorkerCount(t *testing.T) {
	for _, args := range [][]string{{"--frobnicate"}, {"--workers", "lots"}, {"--workers"}} {
		r := runE2E(t, greenRun(), args...)
		if r.exit != 2 {
			t.Errorf("%v: expected exit 2, got %d", args, r.exit)
		}
		if strings.Contains(r.calls, "network create") {
			t.Errorf("%v: must not claim the lock before validating its arguments", args)
		}
	}
}

// --- the happy path --------------------------------------------------------

func TestE2ERunsTheWholeSequenceUnderTheLockAndReleasesIt(t *testing.T) {
	r := runE2E(t, greenRun())
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}

	// The lock is claimed before anything destructive, and it carries the
	// holder's working directory so a refusal elsewhere can name it.
	if !strings.Contains(r.calls, "network create inventory-e2e-gate-lock") {
		t.Error("the lock was never claimed")
	}
	mustContain(t, r.calls, "inventory.e2e.lock.working_dir="+r.root, "the lock names its holder")
	mustContain(t, r.calls, "inventory.e2e.lock.claimed_epoch=", "the lock carries a claim time")

	claim := strings.Index(r.calls, "network create")
	firstDown := strings.Index(r.calls, "down -v")
	if claim < 0 || firstDown < 0 || claim > firstDown {
		t.Errorf("the lock must be claimed before the opening down -v (claim at %d, down at %d)", claim, firstDown)
	}

	for _, want := range []string{
		"down -v --remove-orphans",
		"compose -p inventory-e2e -f docker-compose.e2e.yml build",
		"run --rm app migrate up",
		"up -d",
		"exec -T db psql -U e2e -d e2e -f /fixtures/seed.sql",
		"healthz",
		"sha256sum",
	} {
		mustContain(t, r.calls, want, "the sequence runs "+want)
	}

	// Released on the way out, every time - the whole reason this command
	// exists rather than a pasted sequence (#377).
	mustContain(t, r.calls, "network rm inventory-e2e-gate-lock", "the lock is released")
	if strings.Count(r.calls, "down -v --remove-orphans") < 2 {
		t.Error("expected a down -v at both ends of the run")
	}
	mustContain(t, r.stdout, "192 passed", "the suite's own counts reach stdout")
}

func TestE2ECompactsTheSuiteOutputAndKeepsTheWholeLogOnDisk(t *testing.T) {
	env := greenRun()
	env["STUB_SUITE_OUT"] = suiteFlaky
	env["STUB_SUITE_RC"] = "1"
	r := runE2E(t, env)
	if r.exit != 1 {
		t.Fatalf("the suite's exit code is the command's: expected 1, got %d", r.exit)
	}

	// The failure header, the counts and the worker line - not the 190 passing
	// test lines, which is the difference between four lines of a reviewer's
	// context and several hundred (the shape scripts/dev.d/test established).
	for _, want := range []string{
		"Running 192 tests using 4 workers",
		"1) specs/products.spec.js:690:3",
		"1 failed",
		"2 flaky",
		"189 passed",
		"exit=1",
	} {
		mustContain(t, r.stdout, want, "the summary carries "+want)
	}
	mustNotContain(t, r.stdout, "the shell renders", "a passing test's own line stays in the log")

	full, err := os.ReadFile(filepath.Join(r.root, ".claude", "last-e2e.log"))
	if err != nil {
		t.Fatalf("the full log must be on disk: %v", err)
	}
	mustContain(t, string(full), "the shell renders", "the log has what stdout dropped")
}

// Workers and retries are passed to the run, never baked into
// playwright.config.js, so CI - which passes neither - keeps its own defaults
// and zero retries (#382).
func TestE2ECapsWorkersLocallyAndLetsThemBeOverridden(t *testing.T) {
	r := runE2E(t, greenRun())
	mustContain(t, r.calls, "-e E2E_WORKERS=4", "the local default caps the worker count")
	mustContain(t, r.calls, "-e E2E_RETRIES=1", "a local run retries once so a flake reads as a flake")
	mustContain(t, r.stdout, "workers:  4, retries: 1", "the run says what it used")

	r = runE2E(t, greenRun(), "--workers", "2")
	mustContain(t, r.calls, "-e E2E_WORKERS=2", "--workers overrides the default")

	env := greenRun()
	env["E2E_WORKERS"] = "8"
	env["E2E_RETRIES"] = "0"
	r = runE2E(t, env)
	mustContain(t, r.calls, "-e E2E_WORKERS=8", "the environment overrides the default")
	mustContain(t, r.calls, "-e E2E_RETRIES=0", "retries can be turned off locally too")
}

// The image tag is derived from the checkout, so a second checkout's build
// cannot occupy the tag this stack runs (#391).
func TestE2ETagsTheImagePerCheckout(t *testing.T) {
	a := runE2E(t, greenRun())
	b := runE2E(t, greenRun())

	tagOf := func(r result) string {
		for _, line := range strings.Split(r.stdout, "\n") {
			if strings.HasPrefix(line, "image:") {
				return strings.TrimSpace(strings.TrimPrefix(line, "image:"))
			}
		}
		t.Fatalf("no image line in:\n%s", r.stdout)
		return ""
	}
	tagA, tagB := tagOf(a), tagOf(b)
	if !strings.HasPrefix(tagA, "inventory-e2e-app:") {
		t.Errorf("the tag must stay under the inventory-e2e-app repository, got %q", tagA)
	}
	if tagA == tagB {
		t.Errorf("two checkouts must not share an image tag, both got %q", tagA)
	}
	// Nothing Docker forbids in a tag: letters, digits and _ . - only.
	bad := strings.TrimLeft(strings.TrimPrefix(tagA, "inventory-e2e-app:"),
		"abcdefghijklmnopqrstuvwxyz0123456789_.-")
	if bad != "" {
		t.Errorf("tag %q contains characters Docker does not allow: %q", tagA, bad)
	}
}

// --- the asset self-check --------------------------------------------------

func TestE2ERefusesToRunTheSuiteAgainstAForeignFrontend(t *testing.T) {
	env := greenRun()
	env["STUB_SERVED"] = "/js/api.js " + manifestDigestA + "\n/js/dom.js " + foreignDigestB
	r := runE2E(t, env)
	if r.exit == 0 {
		t.Fatal("a digest mismatch must fail the gate")
	}

	// The whole point is that this is legible instead of five inexplicable
	// test failures, so the message has to name the path, both digests and how
	// to tell a foreign image from a stale manifest.
	mustContain(t, r.stderr, "not this checkout's", "the message leads with what went wrong")
	mustContain(t, r.stderr, "/js/dom.js", "the mismatching path is named")
	mustContain(t, r.stderr, foreignDigestB, "the served digest is named")
	mustContain(t, r.stderr, manifestDigestB, "the expected digest is named")
	mustContain(t, r.stderr, "scripts/dev test ./web/", "the message says how to tell the two causes apart")
	mustContain(t, r.stderr, "inventory-e2e-app:", "the message names the image this checkout expects")

	// And it fails BEFORE the suite, which is what saves the reviewer from
	// reading another checkout's frontend as a regression in the diff.
	if strings.Contains(r.calls, "-e E2E_WORKERS=") {
		t.Error("the suite must not run once the served frontend is known to be foreign")
	}
	mustContain(t, r.calls, "network rm inventory-e2e-gate-lock", "a refused run still releases the lock")
}

func TestE2ETreatsAnUnreachableAssetAsAMismatch(t *testing.T) {
	env := greenRun()
	env["STUB_SERVED"] = "/js/api.js " + manifestDigestA + "\n/js/dom.js unreachable"
	r := runE2E(t, env)
	if r.exit == 0 {
		t.Fatal("an asset that could not be fetched must fail the gate, not pass it")
	}
	mustContain(t, r.stderr, "/js/dom.js", "the unreachable path is named")
}

func TestE2EFailsLoudlyWhenTheManifestHasNoJSDigests(t *testing.T) {
	env := greenRun()
	env["TEST_MANIFEST"] = `{"cache_version":"v1","digests":{}}`
	r := runE2E(t, env)
	if r.exit == 0 {
		t.Fatal("expected a failure when no /js/ digests can be parsed")
	}
	mustContain(t, r.stderr, "web/shell-manifest.json", "the message names the file it could not read digests from")
}

// --- refusals --------------------------------------------------------------

func TestE2ERefusesAnActiveGateAndTouchesNothing(t *testing.T) {
	r := runE2E(t, foreignActive())
	if r.exit != 3 {
		t.Fatalf("an active gate must exit 3 (wait for it), got %d", r.exit)
	}
	mustContain(t, r.stderr, "wait for it", "the refusal says to wait")
	mustContain(t, r.stderr, "Inventory-somewhere-else", "the refusal names the holder's directory")
	mustContain(t, r.stderr, "verdict:  ACTIVE", "the refusal states its verdict")
	mustContain(t, r.stderr, "inventory-e2e-e2e-run-9f3", "the runner container is the evidence")

	// The invariant that makes the lock worth having: a refused session does
	// not touch the holder's stack, nor its lock (#389).
	for _, forbidden := range []string{"down -v", "network rm", "up -d", "build"} {
		if strings.Contains(r.calls, forbidden) {
			t.Errorf("a refused run ran %q against another checkout's stack:\n%s", forbidden, r.calls)
		}
	}
}

func TestE2ERefusesAStaleGateWithADifferentCodeAndNamesTheRemedy(t *testing.T) {
	r := runE2E(t, foreignStale())
	if r.exit != 4 {
		t.Fatalf("a stale gate must exit 4 (break it), got %d", r.exit)
	}
	mustContain(t, r.stderr, "verdict:  STALE", "the refusal states its verdict")
	mustContain(t, r.stderr, "scripts/dev e2e break-lock", "the refusal names the one command that clears it")
	mustContain(t, r.stderr, "no container for the e2e service", "the missing runner is reported as evidence")
	mustContain(t, r.stderr, "silent for over 5m", "the app's silence is reported as evidence")

	// Still nothing destructive: breaking a hold stays deliberate, which is
	// #389's own acceptance criterion.
	for _, forbidden := range []string{"down -v", "network rm"} {
		if strings.Contains(r.calls, forbidden) {
			t.Errorf("a stale gate must not be broken automatically, but ran %q", forbidden)
		}
	}
}

// A lock left behind by THIS checkout with nothing running behind it is its own
// abandoned run - the hard-kill case, since an ordinary interrupt releases the
// lock from the trap - so it is reclaimed rather than refused. Otherwise a
// session would be sent to break a lock that names itself.
func TestE2EReclaimsItsOwnLockWhenNothingIsRunningBehindIt(t *testing.T) {
	env := greenRun()
	env["TEST_PRESEED_LOCK"] = "self-stale"
	r := runE2E(t, env)
	if r.exit != 0 {
		t.Fatalf("expected the run to proceed, got exit %d\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "reclaiming this checkout's own lock", "it says what it did")
	mustContain(t, r.calls, "-e E2E_WORKERS=", "and the suite actually ran")
	mustContain(t, r.calls, "network rm inventory-e2e-gate-lock", "and the reclaimed lock is released")
}

// The other half, and the one that matters: a lock this checkout wrote is NOT
// on its own evidence that the run behind it is over. Two terminals in one
// checkout write the same working_dir, so taking a matching label as "my own
// abandoned run" let the second one `down -v` the first one's live suite —
// exactly the failure the lock exists to prevent, readmitted through the one
// branch that skipped the check. Raised by review-go on PR #469 round 1.
func TestE2EWillNotReclaimItsOwnLockWhileASuiteIsRunningBehindIt(t *testing.T) {
	env := greenRun()
	env["TEST_PRESEED_LOCK"] = "self-active"
	// Terminal A is mid-suite: a container for the `e2e` service exists and the
	// app is being hit constantly.
	env["STUB_RUNNER"] = "inventory-e2e-e2e-run-aa1 (Up 4 minutes)"
	env["STUB_APP"] = "inventory-e2e-app-1"
	env["STUB_APP_RECENT"] = "1"

	r := runE2E(t, env)
	if r.exit != 3 {
		t.Fatalf("terminal B must be refused with exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stderr, "THIS checkout already holds the gate",
		"the refusal distinguishes itself from the foreign-holder case")
	mustContain(t, r.stderr, "break-lock --force", "and names the deliberate override")
	mustNotContain(t, r.stdout, "reclaiming", "it must not claim to be reclaiming an abandoned run")

	// The whole point: terminal A's stack and lock survive untouched.
	for _, forbidden := range []string{"down -v", "build", "up -d", "network rm"} {
		if strings.Contains(r.calls, forbidden) {
			t.Errorf("a second terminal in the same checkout ran %q against a live run:\n%s", forbidden, r.calls)
		}
	}
}

// A young lock of our own with no runner yet is the gap between `up -d` and the
// suite's first request in the other terminal - still a live run, still not
// ours to reclaim.
func TestE2EWillNotReclaimItsOwnYoungLockEvenBeforeTheRunnerExists(t *testing.T) {
	env := greenRun()
	env["TEST_PRESEED_LOCK"] = "self-active"
	r := runE2E(t, env)
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	if strings.Contains(r.calls, "down -v") {
		t.Error("a young lock of our own must not be torn down")
	}
}

// cmd_down has the same hole if it only checks the holder's name: a `down` from
// a second terminal in the same checkout would destroy the first's live suite.
func TestE2EDownRefusesItsOwnLockWhileASuiteIsRunning(t *testing.T) {
	env := map[string]string{
		"TEST_PRESEED_LOCK": "self-active",
		"STUB_RUNNER":       "inventory-e2e-e2e-run-aa1 (Up 4 minutes)",
		"STUB_APP":          "inventory-e2e-app-1",
		"STUB_APP_RECENT":   "1",
	}
	r := runE2E(t, env, "down")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d\n%s", r.exit, r.stdout)
	}
	mustContain(t, r.stderr, "would destroy it", "it says what it refused to do")
	mustContain(t, r.stderr, "break-lock --force", "and names the deliberate override")
	for _, forbidden := range []string{"down -v", "network rm"} {
		if strings.Contains(r.calls, forbidden) {
			t.Errorf("down ran %q against this checkout's own live run", forbidden)
		}
	}
}

// E2E_RETRIES is validated in the wrapper, like --workers, rather than only by
// playwright.config.js inside the container - which would spend a whole
// reset/build/migrate/up cycle before reporting a typo.
func TestE2ERejectsANonNumericRetryCountBeforeClaimingTheGate(t *testing.T) {
	env := greenRun()
	env["E2E_RETRIES"] = "once"
	r := runE2E(t, env)
	if r.exit != 2 {
		t.Fatalf("expected exit 2, got %d", r.exit)
	}
	mustContain(t, r.stderr, "E2E_RETRIES must be a number", "it names the variable and the problem")
	if strings.Contains(r.calls, "network create") || strings.Contains(r.calls, "build") {
		t.Error("a bad retry count must be caught before the lock is claimed and anything is built")
	}
}

// Every exit path tears down and releases, because the teardown is a trap and
// not the last line of a sequence (#377). A step that fails mid-sequence is
// the case that used to leave a stack up.
func TestE2EStillTearsDownWhenAStepFails(t *testing.T) {
	env := greenRun()
	env["STUB_FAIL_STEP"] = "up -d"
	r := runE2E(t, env)
	if r.exit != 17 {
		t.Fatalf("expected the failing step's own exit code 17, got %d", r.exit)
	}
	mustContain(t, r.stderr, "start the stack failed", "the failing step is named")
	mustContain(t, r.stderr, "last-e2e.log", "and the log is pointed at")
	mustContain(t, r.calls, "down -v --remove-orphans", "a failed run still tears down")
	mustContain(t, r.calls, "network rm inventory-e2e-gate-lock", "a failed run still releases the lock")
	if strings.Contains(r.calls, "-e E2E_WORKERS=") {
		t.Error("the suite must not run after a step failed")
	}
}

// A claim can fail for reasons that are not a held lock - the daemon is down,
// or out of address pools. Saying "the gate is locked, break it" then would
// send a session after a lock that does not exist.
func TestE2ESaysSoWhenTheDaemonRefusesTheClaimForAnotherReason(t *testing.T) {
	env := greenRun()
	// The stub refuses every `network create` while reporting no lock at all,
	// which is exactly the shape a dead daemon or an exhausted address pool has.
	env["STUB_REFUSE_CREATE"] = "1"
	r := runE2E(t, env)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "there is no lock either", "it does not blame a lock that is absent")
	mustContain(t, r.stderr, "docker network create", "it names the command to run to see why")
	mustNotContain(t, r.stderr, "break-lock", "and it does not send anyone after a nonexistent lock")
	for _, forbidden := range []string{"down -v", "build", "up -d"} {
		if strings.Contains(r.calls, forbidden) {
			t.Errorf("a run that never claimed the gate ran %q", forbidden)
		}
	}
}

// --- status ----------------------------------------------------------------

func TestE2EStatusReportsAFreeGate(t *testing.T) {
	r := runE2E(t, nil, "status")
	if r.exit != 0 {
		t.Fatalf("a free gate must exit 0, got %d", r.exit)
	}
	mustContain(t, r.stdout, "verdict:  FREE", "a free gate says so")
	if strings.Contains(r.calls, "network create") {
		t.Error("status must not claim the lock")
	}
}

func TestE2EStatusReportsAllThreeDiscriminators(t *testing.T) {
	env := foreignStale()
	// The third discriminator is the holder's own worklog, which status reads
	// from the path the lock records. A real one exists on this machine; here
	// the holder is pointed at a directory this test writes.
	holder := t.TempDir()
	if err := os.MkdirAll(filepath.Join(holder, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	worklog := "# Worklog\nUpdated: 2026-09-29T08:51:00Z\nIssue:   #275\nPhase:   merged and reported\n"
	if err := os.WriteFile(filepath.Join(holder, ".claude", "worklog.md"), []byte(worklog), 0o644); err != nil {
		t.Fatal(err)
	}
	env["STUB_LOCK_DIR"] = filepath.ToSlash(holder)

	r := runE2E(t, env, "status")
	if r.exit != 4 {
		t.Fatalf("a stale gate must exit 4, got %d", r.exit)
	}
	mustContain(t, r.stdout, "runner:   none", "discriminator 1: is there a suite container at all")
	mustContain(t, r.stdout, "silent for over 5m", "discriminator 2: has the app logged anything lately")
	mustContain(t, r.stdout, "Phase:   merged and reported", "discriminator 3: the holder's own worklog, verbatim")
	mustContain(t, r.stdout, "Up 50 minutes (healthy)", "the container list is still reported")
	mustContain(t, r.stdout, "break-lock", "status names the remedy")
}

func TestE2EStatusCallsALiveRunActiveOnTheRunnerContainerAlone(t *testing.T) {
	env := foreignActive()
	// An old lock and a silent app: the runner container on its own is enough
	// to say a suite is executing, which is the case where waiting is right
	// and tearing down destroys a real run.
	env["STUB_LOCK_AGE"] = "5000"
	env["STUB_APP_RECENT"] = "0"
	r := runE2E(t, env, "status")
	if r.exit != 3 {
		t.Fatalf("expected exit 3 for an active gate, got %d", r.exit)
	}
	mustContain(t, r.stdout, "verdict:  ACTIVE", "a runner container alone means ACTIVE")
	mustContain(t, r.stdout, "Wait for it", "and the advice is to wait")
}

// A lock younger than the stale threshold is never called stale, even with no
// runner and a quiet app: that is the gap between `up -d` and the suite's
// first request, where `npm install` can be silent for a minute or more.
func TestE2EStatusWillNotCallAYoungLockStale(t *testing.T) {
	env := foreignStale()
	env["STUB_LOCK_AGE"] = "30"
	r := runE2E(t, env, "status")
	if r.exit != 3 {
		t.Fatalf("a 30-second-old lock must not be reported stale, got exit %d", r.exit)
	}
	mustContain(t, r.stdout, "verdict:  ACTIVE", "a young lock is ACTIVE")
}

// --- break-lock and down ---------------------------------------------------

func TestE2EBreakLockRefusesALiveRunUnlessForced(t *testing.T) {
	r := runE2E(t, foreignActive(), "break-lock")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d", r.exit)
	}
	mustContain(t, r.stderr, "refusing to break an ACTIVE gate", "it says why it refused")
	mustContain(t, r.stderr, "--force", "it names the deliberate override")
	for _, forbidden := range []string{"down -v", "network rm"} {
		if strings.Contains(r.calls, forbidden) {
			t.Errorf("a refused break-lock ran %q", forbidden)
		}
	}

	r = runE2E(t, foreignActive(), "break-lock", "--force")
	if r.exit != 0 {
		t.Fatalf("--force must break it, got exit %d", r.exit)
	}
	mustContain(t, r.calls, "down -v --remove-orphans", "--force clears the stack")
	mustContain(t, r.calls, "network rm inventory-e2e-gate-lock", "--force releases the lock")
}

func TestE2EBreakLockClearsAStaleHold(t *testing.T) {
	r := runE2E(t, foreignStale(), "break-lock")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "verdict:  STALE", "it shows the evidence it acted on")
	mustContain(t, r.calls, "down -v --remove-orphans", "the leftover stack goes with the lock")
	mustContain(t, r.calls, "network rm inventory-e2e-gate-lock", "the lock is removed")
	mustContain(t, r.stdout, "gate released", "it says the gate is free again")
}

func TestE2EBreakLockOnAFreeGateIsHarmless(t *testing.T) {
	r := runE2E(t, nil, "break-lock")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.stderr, "no lock to break", "it says there was nothing to do")
	if strings.Contains(r.calls, "down -v") {
		t.Error("nothing to break means nothing to tear down")
	}
}

func TestE2EBreakLockOnAFreeGateStillReportsLeftoverContainers(t *testing.T) {
	r := runE2E(t, map[string]string{
		"STUB_STACK": "inventory-e2e-app-1  Up 2 hours  C:\\Users\\tizia\\Projekte\\Inventory",
	}, "break-lock")
	mustContain(t, r.stderr, "inventory-e2e-app-1", "a pre-lock leftover stack is still reported")
	mustContain(t, r.stderr, "scripts/dev e2e down", "and the command that clears it is named")
}

func TestE2EDownRefusesAnotherCheckoutsLock(t *testing.T) {
	r := runE2E(t, foreignStale(), "down")
	if r.exit != 3 {
		t.Fatalf("expected exit 3, got %d", r.exit)
	}
	mustContain(t, r.stderr, "held by", "it names the holder")
	mustContain(t, r.stderr, "break-lock", "it points at the deliberate route")
	if strings.Contains(r.calls, "down -v") {
		t.Error("down must not tear down another checkout's stack")
	}
}

func TestE2EDownTearsDownAndReleasesOnAFreeGate(t *testing.T) {
	r := runE2E(t, nil, "down")
	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.calls, "down -v --remove-orphans", "it tears the stack down")
	mustContain(t, r.calls, "network rm inventory-e2e-gate-lock", "it releases the lock")
}

// --- the readiness poll ----------------------------------------------------

func TestE2EStopsWhenTheAppNeverAnswersHealthz(t *testing.T) {
	env := greenRun()
	env["STUB_HEALTHZ_RC"] = "1"
	r := runE2E(t, env)
	if r.exit == 0 {
		t.Fatal("a stack that never answers /healthz must not reach the suite")
	}
	mustContain(t, r.stderr, "wait for /healthz failed", "the failing step is named")
	if strings.Contains(r.calls, "-e E2E_WORKERS=") {
		t.Error("the suite must not run against a stack that never became ready")
	}
	mustContain(t, r.calls, "network rm inventory-e2e-gate-lock", "and the lock is still released")
}
