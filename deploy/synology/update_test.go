// Package synology carries no application code: this directory holds the NAS
// deployment scripts. The test lives here because `update` runs unattended on
// the operator's NAS against a live stack, and `docker compose run --rm app go
// test ./...` (docs/specs/01-architecture-and-deployment.md) is the only test
// runner this project has - there is no host toolchain to run a shell test
// framework with.
//
// The script reaches the outside world through exactly three commands: docker,
// docker-compose and git. Every scenario below puts a recording stub for each of
// them first on PATH, runs the real script against a throwaway copy of the
// clone, and asserts on what the script asked those three to do. No Docker
// daemon, no clone and no stack are involved, so the suite is as fast and as
// deterministic as the rest of `go test ./...` - and the script runs under the
// same busybox `sh` and the same busybox utilities the DSM has, because the dev
// image is Alpine.
//
// What this cannot prove is anything about the real daemon's or the real
// Compose's behaviour. The stubs encode what Compose 2.31.0 - the standalone
// version spec 01 tells the operator to install - was observed to do: the
// `com.docker.compose.oneoff` label, the shape of a `--dry-run` plan line, and
// the exact wording of the orphan-containers warning. They do not encode what
// DSM's own binary does. That distinction is in deploy/synology/README.md,
// "Verifying a change to these scripts".
package synology

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// The container ids the scenarios use. Real ids are 64 hex characters and the
// script only ever compares them as opaque strings, so readable stand-ins do.
const (
	idOld    = "aaaa000000000000000000000000000000000000000000000000000000000001"
	idNew    = "bbbb000000000000000000000000000000000000000000000000000000000002"
	idOneOff = "cccc000000000000000000000000000000000000000000000000000000000003"
)

// Recording stubs. Each appends its whole argument list to $STUB_LOG before
// answering, and takes its answers from STUB_* variables the scenario sets, so
// one stub serves them all.
//
// `case` arms match in order, so the specific compose sub-commands come before
// the general `up -d` arm. Answers that have to change between two calls of the
// same command are indexed by a call counter kept in a file: STUB_PSQ_2 is what
// the second `ps -q app` returns, and STUB_PSQ the answer for any call with no
// index of its own.
const composeStub = `#!/bin/sh
printf 'compose %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  *"version --short"*)
    echo "${STUB_COMPOSE_VERSION:-2.31.0}"; exit 0 ;;
  *"config --services"*)
    printf '%s\n' ${STUB_SERVICES:-app db ts-inventory}; exit 0 ;;
  *"ps -q app"*)
    n=$(cat "$STUB_DIR/psq" 2>/dev/null || echo 0)
    n=$((n + 1)); echo "$n" > "$STUB_DIR/psq"
    eval "ids=\${STUB_PSQ_$n-\$STUB_PSQ}"
    [ -z "$ids" ] || printf '%s\n' $ids
    exit 0 ;;
  *"exec -T db sh -c"*)
    printf '%s\n' "${STUB_PENDING-0}"; exit ${STUB_PENDING_RC:-0} ;;
  *"exec -T db wget"*)
    exit ${STUB_HEALTH_RC:-0} ;;
  *"--dry-run"*)
    [ -z "${STUB_PLAN:-}" ] || printf '%s\n' "$STUB_PLAN"
    exit ${STUB_PLAN_RC:-0} ;;
  *"migrate up"*)
    exit ${STUB_MIGRATE_RC:-0} ;;
  *"stop ts-inventory app"*)
    exit ${STUB_STOP_RC:-0} ;;
  *"force-recreate ts-inventory"*)
    exit ${STUB_SIDECAR_RC:-0} ;;
  *"--scale app=2 app"*)
    exit ${STUB_SCALEUP_RC:-0} ;;
  *"up -d"*)
    exit ${STUB_UP_RC:-0} ;;
esac
exit 0
`

// The docker stub models the one daemon behaviour the item-2 fix depends on:
// `docker ps` honours --filter, so a call that asks for
// com.docker.compose.oneoff=False does not get a one-off container back. Both
// lists are set per scenario, and a script that dropped the filter would be
// answered with the unfiltered one - which is what makes the assertions bite.
const dockerStub = `#!/bin/sh
printf 'docker %s\n' "$*" >> "$STUB_LOG"
case "$1" in
  ps)
    n=$(cat "$STUB_DIR/dps" 2>/dev/null || echo 0)
    n=$((n + 1)); echo "$n" > "$STUB_DIR/dps"
    case "$*" in
      *"oneoff=False"*) eval "ids=\${STUB_APP_SERVICE_IDS_$n-\$STUB_APP_SERVICE_IDS}" ;;
      *)                eval "ids=\${STUB_APP_ALL_IDS_$n-\$STUB_APP_ALL_IDS}" ;;
    esac
    [ -z "$ids" ] || printf '%s\n' $ids
    exit 0 ;;
  inspect)
    case "$*" in
      *State.Running*) echo "${STUB_RUNNING:-true}" ;;
      *IPAddress*)     echo "${STUB_IP:-172.28.0.9}" ;;
      *Config.Env*)    echo "HTTP_PORT=${STUB_APP_PORT:-8000}" ;;
    esac
    exit 0 ;;
  stop) exit ${STUB_DOCKER_STOP_RC:-0} ;;
  rm)   exit ${STUB_DOCKER_RM_RC:-0} ;;
esac
exit 0
`

// git is stubbed for the same reason docker is: the pull path has to run without
// a repository. `rev-parse HEAD` answers from a call counter, so a scenario can
// make the pull move HEAD.
const gitStub = `#!/bin/sh
printf 'git %s\n' "$*" >> "$STUB_LOG"
case "$1" in
  diff)
    [ -z "${STUB_GIT_DIFF_MSG:-}" ] || echo "$STUB_GIT_DIFF_MSG" >&2
    exit ${STUB_GIT_DIFF_RC:-0} ;;
  pull)
    exit ${STUB_GIT_PULL_RC:-0} ;;
  rev-parse)
    case "$*" in
      *--abbrev-ref*) echo "${STUB_GIT_BRANCH:-main}"; exit 0 ;;
      *--short*)
        for a in "$@"; do :; done
        printf '%s' "$a" | cut -c1-7; exit 0 ;;
    esac
    n=$(cat "$STUB_DIR/head" 2>/dev/null || echo 0)
    n=$((n + 1)); echo "$n" > "$STUB_DIR/head"
    eval "sha=\${STUB_HEAD_$n-\$STUB_HEAD}"
    printf '%s\n' "${sha:-1111111111111111111111111111111111111111}"
    exit 0 ;;
esac
exit 0
`

// projectSeq keeps every scenario on its own Compose project name, so the lock
// directory of one cannot collide with another's.
var projectSeq int64

type result struct {
	exit   int
	stdout string
	stderr string
	calls  string // every stub invocation, in order, one per line
}

func (r result) called(substr string) bool { return strings.Contains(r.calls, substr) }

// run copies the real script into a throwaway clone, writes the stubs, and runs
// it. env holds both the script's own variables (HEALTH_TIMEOUT, ...) and the
// STUB_* ones the stubs read.
func run(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()

	script, err := os.ReadFile("update")
	if err != nil {
		t.Fatalf("read the script under test: %v", err)
	}

	root := t.TempDir()
	// The script locates the clone as $(dirname $0)/../.., so the copy has to sit
	// at that depth for ROOT and the -f paths to come out the way they do on the
	// NAS.
	dir := filepath.Join(root, "deploy", "synology")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "update")
	if err := os.WriteFile(path, script, 0o755); err != nil {
		t.Fatal(err)
	}

	stubs := filepath.Join(root, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"docker-compose": composeStub,
		"docker":         dockerStub,
		"git":            gitStub,
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	project := env["INVENTORY_PROJECT"]
	if project == "" {
		project = fmt.Sprintf("t%d", atomic.AddInt64(&projectSeq, 1))
	}
	log := filepath.Join(root, "calls.log")

	// The stubs come first on PATH; the rest of it is the image's own, because
	// the script's sed, grep, tr, date and mkdir have to be the real ones.
	vars := map[string]string{
		"PATH":              stubs + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"STUB_LOG":          log,
		"STUB_DIR":          root,
		"INVENTORY_PROJECT": project,
		// Every stub answers on the first probe, so neither wait loop sleeps.
		"HEALTH_TIMEOUT": "10",
		"DRAIN_TIMEOUT":  "10",
	}
	for k, v := range env {
		vars[k] = v
	}
	cmd := exec.Command("/bin/sh", append([]string{path}, args...)...)
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
			t.Fatalf("running the script: %v", err)
		}
		exit = ee.ExitCode()
	}

	calls, _ := os.ReadFile(log)
	r := result{exit: exit, stdout: stdout.String(), stderr: stderr.String(), calls: string(calls)}
	t.Logf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s--- calls ---\n%s",
		r.exit, r.stdout, r.stderr, r.calls)
	return r
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

// rollingEnv is a rolling update that succeeds: one old instance, a plan that
// says something would be recreated, no pending job, a healthy new instance.
func rollingEnv() map[string]string {
	return map[string]string{
		"STUB_PLAN":            " DRY-RUN MODE -  Container probe-app-1  Recreate",
		"STUB_PSQ_1":           idOld,
		"STUB_PSQ_2":           idOld,
		"STUB_PSQ_3":           idOld + " " + idNew,
		"STUB_PSQ":             idOld + " " + idNew,
		"STUB_APP_SERVICE_IDS": idOld,
		"STUB_APP_ALL_IDS":     idOld,
		"STUB_PENDING":         "0",
	}
}

// TestRollingUpdateHappyPath is what makes the negative scenarios mean
// something: it pins the order the script drives Compose in, so a stub that had
// drifted out of step with the script fails here first.
func TestRollingUpdateHappyPath(t *testing.T) {
	r := run(t, rollingEnv(), "--no-pull")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	for _, step := range []string{
		"migrate up",
		"exec -T db sh -c",
		"--scale app=2 app",
		"exec -T db wget",
		"docker stop -t 30 " + idOld,
		"docker rm -f " + idOld,
		"--scale app=1 app",
		"--force-recreate ts-inventory",
	} {
		mustContain(t, r.calls, step, "the rolling sequence")
	}
	mustContain(t, r.stdout, "Done.", "stdout")
	// --no-pull consults git for nothing at all.
	mustNotContain(t, r.calls, "git ", "the calls")
}

// Item 1: the EXIT trap removes every app container that is not the id it was
// armed with, so it must never be armed with an id that is no longer running.
func TestRefusesWhenTheAppWasRecreatedDuringTheBuild(t *testing.T) {
	env := rollingEnv()
	// The pre-flight sees the old instance. By the time the re-read happens -
	// after the pull, the build and the migration, minutes later - Docker's
	// restart policy has replaced it with a different container.
	env["STUB_PSQ_2"] = idNew

	r := run(t, env, "--no-pull")

	if r.exit == 0 {
		t.Fatal("expected a refusal, got exit 0")
	}
	mustContain(t, r.stderr, "the app container changed while this run was pulling", "the refusal")
	mustContain(t, r.stderr, "the database is migrated", "the refusal")
	// The whole point: nothing was removed, and no second instance was started.
	mustNotContain(t, r.calls, "docker rm -f", "the calls")
	mustNotContain(t, r.calls, "--scale app=2", "the calls")
}

// Item 2: a concurrent `docker-compose run` container carries oneoff=True and is
// never listed by `ps -q`, so neither the trap nor the pre-flight may treat it
// as an app instance.
func TestOneOffContainersAreNotAppInstances(t *testing.T) {
	t.Run("the trap leaves a concurrent run container alone", func(t *testing.T) {
		env := rollingEnv()
		// The new instance never answers /healthz, so the trap fires while armed.
		env["STUB_HEALTH_RC"] = "1"
		env["HEALTH_TIMEOUT"] = "0"
		// First call is the pre-flight, second is the trap's. The unfiltered
		// answers are what a script without the oneoff filter would see.
		env["STUB_APP_SERVICE_IDS_1"] = idOld
		env["STUB_APP_SERVICE_IDS_2"] = idOld + " " + idNew
		env["STUB_APP_ALL_IDS_1"] = idOld + " " + idOneOff
		env["STUB_APP_ALL_IDS_2"] = idOld + " " + idNew + " " + idOneOff

		r := run(t, env, "--no-pull")

		if r.exit == 0 {
			t.Fatal("expected a failure, got exit 0")
		}
		mustContain(t, r.calls, "docker rm -f "+idNew, "the trap")
		mustNotContain(t, r.calls, "docker rm -f "+idOneOff, "the trap")
		mustContain(t, r.stderr, "the new app instance was removed", "the trap message")
	})

	t.Run("the pre-flight does not call one a stopped app container", func(t *testing.T) {
		env := rollingEnv()
		env["STUB_APP_SERVICE_IDS"] = idOld
		env["STUB_APP_ALL_IDS"] = idOld + " " + idOneOff

		r := run(t, env, "--no-pull")

		if r.exit != 0 {
			t.Fatalf("expected exit 0, got %d", r.exit)
		}
		mustNotContain(t, r.stderr, "left over", "the pre-flight")
	})
}

// Item 3, classic half: everything between the stop and the `up -d` leaves the
// stack stopped, and only a failing migration used to say so.
func TestClassicPathAlwaysReportsTheStoppedStack(t *testing.T) {
	cases := map[string]map[string]string{
		"a failing migration": {"STUB_MIGRATE_RC": "1"},
		"a failing up -d":     {"STUB_UP_RC": "1"},
		"a failing stop":      {"STUB_STOP_RC": "1"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			for k, v := range extra {
				env[k] = v
			}

			r := run(t, env, "--no-pull", "--classic")

			if r.exit == 0 {
				t.Fatal("expected a failure, got exit 0")
			}
			mustContain(t, r.stderr, "the app and the sidecar are STOPPED", "the recovery hint")
			// The hint has to be runnable from anywhere, so it carries both -f
			// files by absolute path.
			mustContain(t, r.stderr, "docker-compose.nas.yml up -d", "the recovery hint")
		})
	}
}

// A classic update that works says nothing about a stopped stack.
func TestClassicPathSaysNothingWhenItWorks(t *testing.T) {
	r := run(t, rollingEnv(), "--no-pull", "--classic")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustNotContain(t, r.stderr, "STOPPED", "stderr")
}

// Item 3, rolling half: between retiring the old instance and recreating the
// sidecar the tailnet URL is down, and the trap is what says so. The SIGHUP the
// issue names reaches the same code through `trap 'exit 129' HUP`; only the
// deterministic failure is executed here.
func TestSidecarFailureReportsTheDeadTailnet(t *testing.T) {
	env := rollingEnv()
	env["STUB_SIDECAR_RC"] = "1"

	r := run(t, env, "--no-pull")

	if r.exit == 0 {
		t.Fatal("expected a failure, got exit 0")
	}
	mustContain(t, r.stderr, "the tailnet URL is down", "the recovery hint")
	mustContain(t, r.stderr, "--force-recreate ts-inventory", "the recovery hint")
	// The update was already committed, so the trap must not remove the new
	// instance - it is the one serving.
	mustNotContain(t, r.calls, "docker rm -f "+idNew, "the trap")
}

// A rolling update that works prints no recovery hint.
func TestRollingUpdateSaysNothingWhenItWorks(t *testing.T) {
	r := run(t, rollingEnv(), "--no-pull")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustNotContain(t, r.stderr, "tailnet URL is down", "stderr")
}

// Item 5: the traefik refusal runs after the pull, so it cannot claim that
// nothing was changed.
func TestTraefikRefusalNamesThePulledCommit(t *testing.T) {
	env := rollingEnv()
	env["STUB_SERVICES"] = "app db ts-inventory traefik"
	env["STUB_HEAD_1"] = "1234567000000000000000000000000000000000"
	env["STUB_HEAD_2"] = "89abcde000000000000000000000000000000000"

	r := run(t, env)

	if r.exit == 0 {
		t.Fatal("expected a refusal, got exit 0")
	}
	mustContain(t, r.stderr, "the merged model lists traefik", "the refusal")
	mustContain(t, r.stderr, "No container was touched.", "the refusal")
	mustContain(t, r.stderr, "moved to 89abcde (from 1234567)", "the refusal")
	mustNotContain(t, r.stderr, "Nothing was changed", "the refusal")
}

// The same refusal claims no more than it knows when there was no pull.
func TestTraefikRefusalWithoutAPullClaimsNothingMore(t *testing.T) {
	env := rollingEnv()
	env["STUB_SERVICES"] = "app db ts-inventory traefik"

	r := run(t, env, "--no-pull")

	if r.exit == 0 {
		t.Fatal("expected a refusal, got exit 0")
	}
	mustContain(t, r.stderr, "No container was touched.", "the refusal")
	mustNotContain(t, r.stderr, "The clone was moved", "the refusal")
}

// Item 6: --prune was honoured only by --classic and the rolling path, although
// every path that reaches the build can leave a dangling image behind.
func TestPruneRunsOnEveryPathThatBuilt(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		args []string
	}{
		"first start": {
			env: map[string]string{
				"STUB_PSQ_1":           "",
				"STUB_PSQ":             "",
				"STUB_APP_SERVICE_IDS": "",
				"STUB_APP_ALL_IDS":     "",
			},
			args: []string{"--no-pull", "--prune"},
		},
		"nothing to swap": {
			env:  map[string]string{"STUB_PLAN": " DRY-RUN MODE -  Container probe-app-1  Running"},
			args: []string{"--no-pull", "--prune"},
		},
		"classic": {args: []string{"--no-pull", "--classic", "--prune"}},
		"rolling": {args: []string{"--no-pull", "--prune"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			for k, v := range c.env {
				env[k] = v
			}

			r := run(t, env, c.args...)

			if r.exit != 0 {
				t.Fatalf("expected exit 0, got %d", r.exit)
			}
			mustContain(t, r.calls, "docker image prune -f", "the prune")
		})
	}
}

// Without the flag, nothing is pruned on any of them.
func TestNothingIsPrunedWithoutTheFlag(t *testing.T) {
	r := run(t, rollingEnv(), "--no-pull")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustNotContain(t, r.calls, "image prune", "the calls")
}

// Nit: the container name sits on the same line as its status, and Compose's
// orphan-containers warning quotes container names too, so a project name or
// clone path containing "start", "stop" or "creat" used to make an unchanged
// stack look like a change on every run.
//
// Every plan below is what Compose 2.31.0 actually printed for the situation
// named, down to the double spaces and the warning's wording; only the project
// name was chosen, to be one that contains "start".
func TestOnlyTheStatusWordDecidesWhetherSomethingChanged(t *testing.T) {
	const warning = `time="2026-09-25T11:03:40+02:00" level=warning ` +
		`msg="Found orphan containers ([startprobe-extra-1]) for this project. ` +
		`If you removed or renamed this service in your compose file, you can run ` +
		`this command with the --remove-orphans flag to clean it up."`

	unchanged := " DRY-RUN MODE -  Container startprobe-ts-inventory-1  Running\n" +
		" DRY-RUN MODE -  Container startprobe-app-1  Running"

	// What Compose printed after a build had produced a new image id.
	changed := " DRY-RUN MODE -  Container startprobe-ts-inventory-1  Recreate\n" +
		" DRY-RUN MODE -  Container startprobe-app-1  Recreate\n" +
		" DRY-RUN MODE -  Container startprobe-app-1  Recreated\n" +
		" DRY-RUN MODE -  Container startprobe-ts-inventory-1  Recreated\n" +
		" DRY-RUN MODE -  Container 93f94a297ce5_startprobe-app-1  Starting\n" +
		" DRY-RUN MODE -  Container 93f94a297ce5_startprobe-app-1  Started"

	cases := map[string]struct {
		plan   string
		planRC string
		swaps  bool
	}{
		"an unchanged stack":                  {plan: unchanged, swaps: false},
		"an unchanged stack with an orphan":   {plan: warning + "\n" + unchanged, swaps: false},
		"a changed stack":                     {plan: changed, swaps: true},
		"a changed stack with an orphan":      {plan: warning + "\n" + changed, swaps: true},
		"nothing but a warning":               {plan: warning, swaps: true},
		"an output in an unknown shape":       {plan: "app: up to date\nts-inventory: up to date", swaps: true},
		"one line that cannot be read":        {plan: unchanged + "\n DRY-RUN MODE -  Container startprobe-db-1", swaps: true},
		"a dry run that failed":               {plan: "", planRC: "1", swaps: true},
		"a status that is neither of the two": {plan: " DRY-RUN MODE -  Container startprobe-app-1  Skipped", swaps: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			env["STUB_PLAN"] = c.plan
			if c.planRC != "" {
				env["STUB_PLAN_RC"] = c.planRC
			}

			r := run(t, env, "--no-pull")

			if r.exit != 0 {
				t.Fatalf("expected exit 0, got %d", r.exit)
			}
			if c.swaps {
				mustContain(t, r.calls, "--scale app=2", "the calls")
				mustNotContain(t, r.stdout, "nothing to swap", "stdout")
				return
			}
			mustContain(t, r.stdout, "nothing to swap", "stdout")
			mustNotContain(t, r.calls, "--scale app=2", "the calls")
		})
	}
}

// --force swaps whatever the plan says.
func TestForceSkipsThePlanAltogether(t *testing.T) {
	env := rollingEnv()
	env["STUB_PLAN"] = " DRY-RUN MODE -  Container probe-app-1  Running"

	r := run(t, env, "--no-pull", "--force")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.calls, "--scale app=2", "the calls")
	mustNotContain(t, r.calls, "--dry-run", "the calls")
}

// Nit: a stale lock used to be indistinguishable from a live one, and on a NAS
// that updates from Task Scheduler that means every later run fails with nothing
// to go on.
func TestLockSaysWhetherItsHolderIsAlive(t *testing.T) {
	// A pid equal to pid_max can never be allocated, so `kill -0` on it always
	// answers "no such process". That is what makes the dead-holder branch
	// deterministic instead of a guess about which pids happen to be free.
	deadPID := "4194304"
	if raw, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			deadPID = strconv.Itoa(n)
		}
	}

	cases := map[string]struct {
		holder string
		expect string
	}{
		// pid 1 always exists inside a container.
		"a live holder":    {holder: "1 started 2026-09-25 10:00:00", expect: "another update is running (1 started"},
		"a dead holder":    {holder: deadPID + " started 2026-09-25 10:00:00", expect: "was killed and left its lock behind"},
		"an empty holder":  {holder: "", expect: "names no process"},
		"a garbage holder": {holder: "not-a-pid", expect: "names no process"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			project := fmt.Sprintf("lock%d", atomic.AddInt64(&projectSeq, 1))
			lock := filepath.Join("/tmp", "inventory-update-"+project+".lock")
			if err := os.MkdirAll(lock, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(lock) })
			if c.holder != "" {
				holder := filepath.Join(lock, "holder")
				if err := os.WriteFile(holder, []byte(c.holder+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			env := rollingEnv()
			env["INVENTORY_PROJECT"] = project

			r := run(t, env, "--no-pull")

			if r.exit == 0 {
				t.Fatal("expected a refusal, got exit 0")
			}
			mustContain(t, r.stderr, c.expect, "the lock refusal")
			mustContain(t, r.stderr, lock, "the lock refusal names the path")
			// A refused run must not take the lock away from whoever holds it.
			if _, err := os.Stat(lock); err != nil {
				t.Errorf("a run that does not own the lock removed it: %v", err)
			}
			// And it must stop before it touches the stack.
			if r.called("compose -p") {
				t.Error("the refused run still talked to Compose about the project")
			}
		})
	}
}

// A run that takes the lock leaves none behind.
func TestLockIsRemovedAfterTheRun(t *testing.T) {
	project := fmt.Sprintf("lock%d", atomic.AddInt64(&projectSeq, 1))
	lock := filepath.Join("/tmp", "inventory-update-"+project+".lock")
	t.Cleanup(func() { os.RemoveAll(lock) })

	env := rollingEnv()
	env["INVENTORY_PROJECT"] = project

	r := run(t, env, "--no-pull")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the lock directory outlived the run: %v", err)
	}
}

// Nit: `git diff --quiet` exits 1 for "there are differences" and something else
// when git itself failed. Only the first is an edit the operator can commit.
func TestGitDiffFailureIsNotReportedAsLocalChanges(t *testing.T) {
	t.Run("differences are local changes", func(t *testing.T) {
		env := rollingEnv()
		env["STUB_GIT_DIFF_RC"] = "1"

		r := run(t, env)

		if r.exit == 0 {
			t.Fatal("expected a refusal, got exit 0")
		}
		mustContain(t, r.stderr, "local changes to tracked files", "the refusal")
		mustContain(t, r.calls, "git status", "the refusal")
	})

	t.Run("git failing is not", func(t *testing.T) {
		env := rollingEnv()
		env["STUB_GIT_DIFF_RC"] = "128"
		env["STUB_GIT_DIFF_MSG"] = "fatal: not a git repository"

		r := run(t, env)

		if r.exit == 0 {
			t.Fatal("expected a refusal, got exit 0")
		}
		mustContain(t, r.stderr, "failed with status 128", "the refusal")
		mustContain(t, r.stderr, "does not look like a usable clone", "the refusal")
		mustNotContain(t, r.stderr, "local changes to tracked files", "the refusal")
		// Nothing was pulled or built on the way out.
		mustNotContain(t, r.calls, "git pull", "the calls")
		mustNotContain(t, r.calls, "build", "the calls")
	})
}

// The pre-flight refusals that predate this change, kept honest by the same
// harness: two running instances, and a genuinely stopped app container.
func TestPreflightStillRefusesTheStatesItAlwaysDid(t *testing.T) {
	t.Run("two running instances", func(t *testing.T) {
		env := rollingEnv()
		env["STUB_PSQ_1"] = idOld + " " + idNew

		r := run(t, env, "--no-pull")

		if r.exit == 0 {
			t.Fatal("expected a refusal, got exit 0")
		}
		mustContain(t, r.stderr, "app instances are running already", "the refusal")
	})

	t.Run("a stopped app container", func(t *testing.T) {
		env := rollingEnv()
		// idNew is a service container that `ps -q` does not list: stopped.
		env["STUB_APP_SERVICE_IDS"] = idOld + " " + idNew

		r := run(t, env, "--no-pull")

		if r.exit == 0 {
			t.Fatal("expected a refusal, got exit 0")
		}
		mustContain(t, r.stderr, "left over from an earlier run", "the refusal")
	})
}

// The drain checks abort rather than continue, which is what the documentation
// now says as well.
func TestPendingJobsAbortTheUpdate(t *testing.T) {
	t.Run("a job that will not drain", func(t *testing.T) {
		env := rollingEnv()
		env["STUB_PENDING"] = "3"
		env["DRAIN_TIMEOUT"] = "0"

		r := run(t, env, "--no-pull")

		if r.exit == 0 {
			t.Fatal("expected a refusal, got exit 0")
		}
		mustContain(t, r.stderr, "3 job(s) still pending", "the refusal")
		mustContain(t, r.stderr, "the database is migrated", "the refusal")
		mustNotContain(t, r.calls, "--scale app=2", "the calls")
	})

	t.Run("a count that cannot be read", func(t *testing.T) {
		env := rollingEnv()
		env["STUB_PENDING"] = ""

		r := run(t, env, "--no-pull")

		if r.exit == 0 {
			t.Fatal("expected a refusal, got exit 0")
		}
		mustContain(t, r.stderr, "cannot read the pending-job count", "the refusal")
		mustContain(t, r.stderr, "--classic", "the refusal")
	})
}

// The version gate everything else depends on.
func TestRefusesAComposeOlderThan224(t *testing.T) {
	env := rollingEnv()
	env["STUB_COMPOSE_VERSION"] = "2.20.1"

	r := run(t, env, "--no-pull")

	if r.exit == 0 {
		t.Fatal("expected a refusal, got exit 0")
	}
	mustContain(t, r.stderr, "is too old", "the refusal")
}
