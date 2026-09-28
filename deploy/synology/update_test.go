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
//
// Each compose call also records the VERSION it was called with, as a trailing
// field: --ref stamps the build with the release's name through the
// environment, and an argument list alone could not show that (spec 38, H15).
// The field goes last so that every assertion about the argument list keeps
// matching.
const composeStub = `#!/bin/sh
printf 'compose %s [VERSION=%s]\n' "$*" "${VERSION-}" >> "$STUB_LOG"
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
  *" build")
    # The build is the one step that fails without going through the script's
    # own die(), so its phase is only reachable with an arm of its own.
    exit ${STUB_BUILD_RC:-0} ;;
  *"run --rm -T backup"*)
    # A backup that is asked to write one writes it where the mount puts it:
    # $STUB_DIR is the throwaway clone's root, so $STUB_DIR/backups is the
    # ./backups the script then looks in. STUB_BACKUP_ARCHIVE empty models a
    # service that exits 0 having written nothing; STUB_BACKUP_UNWRITTEN models
    # one that names an archive it did not put where the script can see it.
    if [ -n "${STUB_BACKUP_ARCHIVE-}" ]; then
      if [ -z "${STUB_BACKUP_UNWRITTEN-}" ]; then
        mkdir -p "$STUB_DIR/backups"
        : > "$STUB_DIR/backups/$STUB_BACKUP_ARCHIVE"
      fi
      echo "backup: wrote /backups/$STUB_BACKUP_ARCHIVE (4.0K)"
    fi
    exit ${STUB_BACKUP_RC:-0} ;;
  *"migrate plan"*)
    # H14's exit codes, stubbed: 0 rolling, 3 classic, anything else an error
    # (docs/specs/38-release-pipeline-and-nas-runner.md, decision D3).
    exit ${STUB_MIGRATE_PLAN_RC:-0} ;;
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
  stop)  exit ${STUB_DOCKER_STOP_RC:-0} ;;
  rm)    exit ${STUB_DOCKER_RM_RC:-0} ;;
  image) exit ${STUB_PRUNE_RC:-0} ;;
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
  fetch)
    exit ${STUB_GIT_FETCH_RC:-0} ;;
  checkout)
    exit ${STUB_GIT_CHECKOUT_RC:-0} ;;
  rev-parse)
    case "$*" in
      *--abbrev-ref*) echo "${STUB_GIT_BRANCH:-main}"; exit 0 ;;
      # "--verify --quiet refs/tags/<ref>" asks whether the ref is a tag, which
      # decides whether the build is stamped with the tag name or the short sha.
      # It has to be matched before the general --verify arm below.
      *refs/tags/*) exit ${STUB_GIT_TAG_RC:-0} ;;
      # "--verify --quiet <ref>^{commit}" is the existence check --ref makes
      # after fetching; a non-zero answer is a ref that does not resolve.
      *--verify*) exit ${STUB_GIT_REF_RC:-0} ;;
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

// The script's retry and wait loops call `sleep`, and it is their own counters,
// not the wall clock, that decide when they give up - so a stub that returns at
// once costs no coverage and saves the suite the real seconds. That is more than
// a convenience here: the Dockerfile's builder stage runs `go test ./...`, so
// every `docker compose build` would otherwise wait them out - and `build` is a
// step of the very script under test, on the operator's NAS.
const sleepStub = `#!/bin/sh
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
	return runIn(t, t.TempDir(), env, args...)
}

// runIn is run against a clone root the scenario made itself, for the cases
// that have to put something in the clone before the run (a ./backups tree, a
// .env) or look at what the run left there afterwards.
func runIn(t *testing.T, root string, env map[string]string, args ...string) result {
	t.Helper()

	script, err := os.ReadFile("update")
	if err != nil {
		t.Fatalf("read the script under test: %v", err)
	}

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
		"sleep":          sleepStub,
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
	cases := map[string]struct {
		env    map[string]string
		expect string
		absent string
	}{
		// The stop returned, so the stack really is stopped and the hint says so.
		"a failing migration": {
			env:    map[string]string{"STUB_MIGRATE_RC": "1"},
			expect: "the app and the sidecar are STOPPED",
		},
		"a failing up -d": {
			env:    map[string]string{"STUB_UP_RC": "1"},
			expect: "the app and the sidecar are STOPPED",
		},
		// The stop itself failed, so nothing may have stopped at all and the app
		// can still be serving. Claiming a stopped stack here would send the
		// operator hunting for one that never stopped.
		"a failing stop": {
			env:    map[string]string{"STUB_STOP_RC": "1"},
			expect: "this run was stopping the app and the sidecar",
			absent: "are STOPPED",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			for k, v := range c.env {
				env[k] = v
			}

			r := run(t, env, "--no-pull", "--classic")

			if r.exit == 0 {
				t.Fatal("expected a failure, got exit 0")
			}
			mustContain(t, r.stderr, c.expect, "the recovery hint")
			if c.absent != "" {
				mustNotContain(t, r.stderr, c.absent, "the recovery hint")
			}
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

// Issue #197: a successful update must not be reported as a failure just
// because the cleanup at the very end of it could not run. All four exit
// paths funnel through maybe_prune, so the rolling path here stands for all
// of them.
func TestFailingPruneDoesNotFailTheUpdate(t *testing.T) {
	env := rollingEnv()
	env["STUB_PRUNE_RC"] = "1"

	r := run(t, env, "--no-pull", "--prune")

	if r.exit != 0 {
		t.Fatalf("a failed prune must not fail an otherwise successful update, got exit %d", r.exit)
	}
	mustContain(t, r.calls, "docker image prune -f", "the prune")
	mustContain(t, r.stderr, "could not prune dangling images", "stderr")
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
	// The test's own process is the live one: always running, and always
	// signalable by the script, which runs as the same user. Pid 1 would need
	// permission to signal init, so a non-root run of this suite would take the
	// "gone" branch and fail for a script defect that is not there.
	livePID := strconv.Itoa(os.Getpid())

	cases := map[string]struct {
		holder string
		expect string
	}{
		"a live holder":    {holder: livePID + " started 2026-09-25 10:00:00", expect: "another update is running (" + livePID + " started"},
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

// Nit: the lock used to live under `$TMPDIR`, so an interactive root shell and a
// Task Scheduler job - which do not share one - did not share a lock either. With
// the path pinned to /tmp, a lock already held there has to stop a run whose
// TMPDIR points somewhere else entirely; a revert of that line would take a second
// lock under TMPDIR and sail past it.
func TestTheLockIgnoresTMPDIR(t *testing.T) {
	project := fmt.Sprintf("lock%d", atomic.AddInt64(&projectSeq, 1))
	lock := filepath.Join("/tmp", "inventory-update-"+project+".lock")
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lock) })
	holder := strconv.Itoa(os.Getpid()) + " started 2026-09-25 10:00:00"
	if err := os.WriteFile(filepath.Join(lock, "holder"), []byte(holder+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := rollingEnv()
	env["INVENTORY_PROJECT"] = project
	// Writable, and empty of any lock: somewhere the run could happily have taken
	// one, if it still looked at TMPDIR.
	env["TMPDIR"] = t.TempDir()

	r := run(t, env, "--no-pull")

	if r.exit == 0 {
		t.Fatal("the run took a lock under TMPDIR instead of seeing the one held in /tmp")
	}
	mustContain(t, r.stderr, "another update is running", "the lock refusal")
	mustContain(t, r.stderr, lock, "the lock refusal names the /tmp path")
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

// ---------------------------------------------------------------------------
// H15 - --ref, --auto, --backup and the summary line
// (docs/specs/38-release-pipeline-and-nas-runner.md, "Acceptance criteria",
// H15; issue #314). Each criterion and the test that covers it:
//
//   --ref fetches, verifies, checks out detached, builds with VERSION=<ref>
//       TestRefFetchesVerifiesAndChecksOutDetached
//       TestRefStampsTheShortShaWhenTheRefIsNotATag
//   --ref refuses a ref that does not resolve, before the build
//       TestRefRefusesAnUnknownRefBeforeTheBuild
//   --ref refuses when git cannot fetch or check out
//       TestRefRefusesWhenGitCannotGetTheCode
//   --ref keeps the dirty-clone refusal
//       TestRefStillRefusesADirtyClone
//   --ref and --no-pull are rejected together
//       TestContradictingOptionsAreRejected
//   --auto takes the mode from migrate plan's exit code
//       TestAutoTakesTheModeFromMigratePlan
//   DEPLOY_MODE=classic forces classic; nothing forces rolling
//       TestDeployModeClassicForcesClassicWithoutAsking
//   --auto and --classic are rejected together
//       TestContradictingOptionsAreRejected
//   --backup runs the backup service before migrate up
//       TestBackupRunsBeforeTheMigrationOnEveryPathThatMigrates
//   --backup aborts on a failed backup and on one that writes no archive
//       TestBackupFailureAbortsBeforeAnythingIsMigrated
//   --backup keeps the newest BACKUP_KEEP archives, never a .part
//       TestBackupRetentionKeepsTheNewestAndNeverAPart
//       TestBackupKeepComesFromTheEnvironmentThenDotEnv
//       TestBackupKeepIsCheckedBeforeTheBackupRuns
//   the summary line, on success and on failure, naming the phase
//       TestSummaryLineEndsEverySuccessfulRun
//       TestSummaryLineNamesThePhaseAFailureStoppedIn — the whole line, not
//       just the phase: mode= is what picks the right half of the two rows
//       more than one path reaches, and mode=unknown is the documented value
//       of every phase that runs before the path is chosen
//   exit 0 on success and on nothing-to-do, 1 on every refusal
//       covered by the exit assertions of all of the above
// ---------------------------------------------------------------------------

// mustPrecede asserts that one recorded call happens before another. Order is
// the whole point of --backup: an archive taken after the schema moved is not a
// pre-upgrade archive.
func mustPrecede(t *testing.T, calls, first, second, what string) {
	t.Helper()
	i := strings.Index(calls, first)
	j := strings.Index(calls, second)
	if i < 0 {
		t.Errorf("%s: %q was never called", what, first)
		return
	}
	if j < 0 {
		t.Errorf("%s: %q was never called", what, second)
		return
	}
	if i > j {
		t.Errorf("%s: %q was called after %q", what, first, second)
	}
}

// lastLine is the summary line: the one line the release pipeline reads
// (docs/specs/38, "What the deploy job runs").
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// refEnv is a rolling update driven by --ref instead of a pull. The two HEADs
// are what the clone stood at before and after the checkout.
func refEnv() map[string]string {
	env := rollingEnv()
	env["STUB_HEAD_1"] = "1111111000000000000000000000000000000000"
	env["STUB_HEAD_2"] = "2222222000000000000000000000000000000000"
	return env
}

func TestRefFetchesVerifiesAndChecksOutDetached(t *testing.T) {
	r := run(t, refEnv(), "--ref", "v1.4.0")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	for _, step := range []string{
		"git fetch origin --tags",
		"git rev-parse --verify --quiet v1.4.0^{commit}",
		"git checkout --detach v1.4.0",
	} {
		mustContain(t, r.calls, step, "the --ref sequence")
	}
	mustPrecede(t, r.calls, "git fetch origin --tags", "git checkout --detach", "the --ref sequence")
	mustPrecede(t, r.calls, "git checkout --detach", "build", "the --ref sequence")
	// --ref replaces the pull; it does not add to it.
	mustNotContain(t, r.calls, "git pull", "the calls")
	mustContain(t, r.stdout, "1111111 -> 2222222", "stdout")
	// The whole reason the stub records its environment: the release is built
	// under its own name, not as "dev".
	mustContain(t, r.calls, "build [VERSION=v1.4.0]", "the build")
	mustContain(t, lastLine(r.stdout), "ref=v1.4.0 version=v1.4.0", "the summary")
}

// A ref that is not a tag - the sha of a commit - deploys under its short sha,
// because there is no name to deploy it under.
func TestRefStampsTheShortShaWhenTheRefIsNotATag(t *testing.T) {
	env := refEnv()
	env["STUB_GIT_TAG_RC"] = "1" // `rev-parse --verify refs/tags/<ref>` finds nothing

	r := run(t, env, "--ref", "9f8e7d6c5b4a392817065432")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustContain(t, r.calls, "build [VERSION=9f8e7d6]", "the build")
	mustContain(t, lastLine(r.stdout), "version=9f8e7d6", "the summary")
}

// Spec 38: "A ref that does not exist ... is a refusal BEFORE anything is built
// or migrated."
func TestRefRefusesAnUnknownRefBeforeTheBuild(t *testing.T) {
	env := refEnv()
	env["STUB_GIT_REF_RC"] = "1"

	r := run(t, env, "--ref", "v9.9.9")

	if r.exit == 0 {
		t.Fatal("expected a refusal, got exit 0")
	}
	mustContain(t, r.stderr, "the ref 'v9.9.9' does not exist", "the refusal")
	mustContain(t, r.stderr, "Nothing was built or migrated", "the refusal")
	mustContain(t, r.calls, "git fetch origin --tags", "the calls")
	mustNotContain(t, r.calls, "git checkout", "the calls")
	mustNotContain(t, r.calls, "build", "the calls")
	mustNotContain(t, r.calls, "migrate up", "the calls")
	mustContain(t, lastLine(r.stdout), "phase=fetch", "the summary")
}

// Spec 38: "The existing refusal to deploy a clone with local changes to
// tracked files stays exactly as it is."
func TestRefStillRefusesADirtyClone(t *testing.T) {
	env := refEnv()
	env["STUB_GIT_DIFF_RC"] = "1"

	r := run(t, env, "--ref", "v1.4.0")

	if r.exit == 0 {
		t.Fatal("expected a refusal, got exit 0")
	}
	mustContain(t, r.stderr, "local changes to tracked files", "the refusal")
	// Refused before the clone was moved anywhere at all.
	mustNotContain(t, r.calls, "git fetch", "the calls")
	mustNotContain(t, r.calls, "git checkout", "the calls")
	mustNotContain(t, r.calls, "build", "the calls")
}

// Two pairs of options say opposite things. Both are rejected before the script
// talks to anything.
func TestContradictingOptionsAreRejected(t *testing.T) {
	cases := map[string]struct {
		args   []string
		expect string
	}{
		"--ref with --no-pull":  {args: []string{"--ref", "v1.4.0", "--no-pull"}, expect: "--ref and --no-pull cannot be combined"},
		"--auto with --classic": {args: []string{"--no-pull", "--auto", "--classic"}, expect: "--classic and --auto cannot be combined"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := run(t, rollingEnv(), c.args...)

			if r.exit == 0 {
				t.Fatal("expected a refusal, got exit 0")
			}
			mustContain(t, r.stderr, c.expect, "the refusal")
			if r.calls != "" {
				t.Errorf("the refused run still called something:\n%s", r.calls)
			}
			mustContain(t, lastLine(r.stdout), "phase=preflight", "the summary")
		})
	}
}

// --ref needs a value, and the option parser says so rather than swallowing the
// next flag as one.
func TestRefWithoutAValueIsRefused(t *testing.T) {
	r := run(t, rollingEnv(), "--ref")

	if r.exit == 0 {
		t.Fatal("expected a refusal, got exit 0")
	}
	mustContain(t, r.stderr, "--ref needs a tag or a commit", "the refusal")
}

const archive = "inventory-backup-2026-09-28-0300.tar.gz"

// Spec 38: --backup runs the backup service "BEFORE migrate up", on every path
// that migrates. The classic case is the sharp one: the failure table promises
// that a failed backup leaves the old release untouched, which is only true
// while nothing has been stopped yet, so the backup runs before the stop too.
func TestBackupRunsBeforeTheMigrationOnEveryPathThatMigrates(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		args []string
		mode string
		// what the backup has to come before on this path
		before []string
	}{
		"rolling": {
			args:   []string{"--no-pull", "--backup"},
			mode:   "rolling",
			before: []string{"migrate up"},
		},
		"classic": {
			args:   []string{"--no-pull", "--classic", "--backup"},
			mode:   "classic",
			before: []string{"stop ts-inventory app", "migrate up"},
		},
		"first start": {
			env: map[string]string{
				"STUB_PSQ_1":           "",
				"STUB_PSQ":             "",
				"STUB_APP_SERVICE_IDS": "",
				"STUB_APP_ALL_IDS":     "",
			},
			args:   []string{"--no-pull", "--backup"},
			mode:   "first-start",
			before: []string{"migrate up"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			for k, v := range c.env {
				env[k] = v
			}
			env["STUB_BACKUP_ARCHIVE"] = archive

			r := run(t, env, c.args...)

			if r.exit != 0 {
				t.Fatalf("expected exit 0, got %d", r.exit)
			}
			mustContain(t, r.calls, "run --rm -T backup", "the backup")
			for _, after := range c.before {
				mustPrecede(t, r.calls, "run --rm -T backup", after, "the backup")
			}
			// The archive the run took is named in the summary, so that the job
			// summary says which one to restore from.
			mustContain(t, lastLine(r.stdout), "mode="+c.mode+" ", "the summary")
			mustContain(t, lastLine(r.stdout), "backup="+archive, "the summary")
		})
	}
}

// Without the flag nothing is backed up, and the summary says so.
func TestNothingIsBackedUpWithoutTheFlag(t *testing.T) {
	r := run(t, rollingEnv(), "--no-pull")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustNotContain(t, r.calls, "run --rm -T backup", "the calls")
	mustContain(t, lastLine(r.stdout), "backup=none", "the summary")
}

// Spec 38: "A failed or missing archive ABORTS the update before the schema is
// touched." Both halves of that are failures: a service that exits non-zero,
// and one that exits 0 having written nothing.
func TestBackupFailureAbortsBeforeAnythingIsMigrated(t *testing.T) {
	cases := map[string]struct {
		env    map[string]string
		args   []string
		expect string
	}{
		"the backup service fails": {
			env:    map[string]string{"STUB_BACKUP_RC": "1", "STUB_BACKUP_ARCHIVE": ""},
			args:   []string{"--no-pull", "--backup"},
			expect: "the backup failed (the backup service exited 1)",
		},
		"the backup writes no archive": {
			env:    map[string]string{"STUB_BACKUP_ARCHIVE": ""},
			args:   []string{"--no-pull", "--backup"},
			expect: "named no archive",
		},
		// The classic path is where an abort could do damage: if the backup ran
		// after the stop, a failed one would leave the stack down.
		"the backup service fails on the classic path": {
			env:    map[string]string{"STUB_BACKUP_RC": "1", "STUB_BACKUP_ARCHIVE": ""},
			args:   []string{"--no-pull", "--classic", "--backup"},
			expect: "the backup failed",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			for k, v := range c.env {
				env[k] = v
			}

			r := run(t, env, c.args...)

			if r.exit == 0 {
				t.Fatal("expected an abort, got exit 0")
			}
			mustContain(t, r.stderr, c.expect, "the abort")
			mustContain(t, r.stderr, "Nothing was migrated", "the abort")
			// The point of the whole feature: the schema was not touched.
			mustNotContain(t, r.calls, "migrate up", "the calls")
			// And on the classic path, neither was the running stack.
			mustNotContain(t, r.calls, "stop ts-inventory app", "the calls")
			mustNotContain(t, r.calls, "--scale app=2", "the calls")
			mustContain(t, lastLine(r.stdout), "phase=backup", "the summary")
			mustContain(t, lastLine(r.stdout), "backup=none", "the summary")
		})
	}
}

// A backup service that names an archive nobody can find is not a backup
// either. That is not a hypothetical: docker-compose.nas.yml exists partly
// because a mount pointing at the wrong place makes the service exit 0 with an
// archive that is not the one anybody wanted.
func TestBackupThatNamesAnArchiveThatIsNotThereAborts(t *testing.T) {
	env := rollingEnv()
	env["STUB_BACKUP_ARCHIVE"] = archive
	env["STUB_BACKUP_UNWRITTEN"] = "1"

	r := run(t, env, "--no-pull", "--backup")

	if r.exit == 0 {
		t.Fatal("expected an abort, got exit 0")
	}
	mustContain(t, r.stderr, "does not hold it", "the abort")
	mustContain(t, r.stderr, "Nothing was migrated", "the abort")
	mustNotContain(t, r.calls, "migrate up", "the calls")
}

// seedBackups makes a ./backups in the clone root and fills it with the names
// given, returning the root.
func seedBackups(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("archive\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// backupsLeft is what ./backups holds after a run.
func backupsLeft(t *testing.T, root string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	left := make(map[string]bool, len(entries))
	for _, e := range entries {
		left[e.Name()] = true
	}
	return left
}

// Spec 38: "afterwards it keeps the newest BACKUP_KEEP archives and deletes the
// older ones", default 5.
//
// The two files that are not archives are the sharp part. A half-written
// archive belongs to a backup that is still running - deleting it would
// sabotage that run - and a file an operator put in ./backups is not this
// script's to retire.
func TestBackupRetentionKeepsTheNewestAndNeverAPart(t *testing.T) {
	const (
		part   = ".inventory-backup-2026-09-28-0400.tar.gz.part"
		theirs = "NOTES.txt"
	)
	root := seedBackups(t,
		"inventory-backup-2026-09-20-0300.tar.gz",
		"inventory-backup-2026-09-21-0300.tar.gz",
		"inventory-backup-2026-09-22-0300.tar.gz",
		"inventory-backup-2026-09-23-0300.tar.gz",
		"inventory-backup-2026-09-24-0300.tar.gz",
		"inventory-backup-2026-09-25-0300.tar.gz",
		part,
		theirs,
	)

	env := rollingEnv()
	env["STUB_BACKUP_ARCHIVE"] = archive // dated 2026-09-28: the newest of the seven

	r := runIn(t, root, env, "--no-pull", "--backup")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	left := backupsLeft(t, root)
	for _, keep := range []string{
		archive,
		"inventory-backup-2026-09-25-0300.tar.gz",
		"inventory-backup-2026-09-24-0300.tar.gz",
		"inventory-backup-2026-09-23-0300.tar.gz",
		"inventory-backup-2026-09-22-0300.tar.gz",
	} {
		if !left[keep] {
			t.Errorf("the newest five have to survive, but %s is gone", keep)
		}
	}
	for _, gone := range []string{
		"inventory-backup-2026-09-21-0300.tar.gz",
		"inventory-backup-2026-09-20-0300.tar.gz",
	} {
		if left[gone] {
			t.Errorf("%s is older than the newest five and should have been removed", gone)
		}
	}
	if !left[part] {
		t.Error("a half-written archive was deleted: a backup that is still running has just been sabotaged")
	}
	if !left[theirs] {
		t.Errorf("%s is not an archive this script wrote, and is not its to remove", theirs)
	}
}

// BACKUP_KEEP is read from the environment first and from .env second - and
// only that one key: sourcing .env would import every other key into the
// script's environment, VERSION among them, which --ref sets on purpose.
func TestBackupKeepComesFromTheEnvironmentThenDotEnv(t *testing.T) {
	seed := []string{
		"inventory-backup-2026-09-24-0300.tar.gz",
		"inventory-backup-2026-09-25-0300.tar.gz",
		"inventory-backup-2026-09-26-0300.tar.gz",
	}

	t.Run("the environment wins", func(t *testing.T) {
		root := seedBackups(t, seed...)
		env := rollingEnv()
		env["STUB_BACKUP_ARCHIVE"] = archive
		env["BACKUP_KEEP"] = "2"

		r := runIn(t, root, env, "--no-pull", "--backup")

		if r.exit != 0 {
			t.Fatalf("expected exit 0, got %d", r.exit)
		}
		if left := backupsLeft(t, root); len(left) != 2 {
			t.Errorf("BACKUP_KEEP=2 should have left two archives, found %d: %v", len(left), left)
		}
	})

	t.Run(".env is read when the environment says nothing", func(t *testing.T) {
		root := seedBackups(t, seed...)
		// VERSION in the same file is the trap this guards: a sourced .env would
		// overwrite the one --ref exports and mislabel the deployed image.
		dotenv := "POSTGRES_USER=inventory\nVERSION=not-the-release\nBACKUP_KEEP=2\n"
		if err := os.WriteFile(filepath.Join(root, ".env"), []byte(dotenv), 0o644); err != nil {
			t.Fatal(err)
		}
		env := refEnv()
		env["STUB_BACKUP_ARCHIVE"] = archive

		r := runIn(t, root, env, "--ref", "v1.4.0", "--backup")

		if r.exit != 0 {
			t.Fatalf("expected exit 0, got %d", r.exit)
		}
		if left := backupsLeft(t, root); len(left) != 2 {
			t.Errorf("BACKUP_KEEP=2 in .env should have left two archives, found %d: %v", len(left), left)
		}
		mustContain(t, r.calls, "build [VERSION=v1.4.0]", "the build")
		mustNotContain(t, r.calls, "not-the-release", "the calls")
	})
}

// A BACKUP_KEEP that is not a number is caught before the backup runs, not
// after: under `set -eu` it would otherwise end the run between a successful
// backup and `migrate up`, which is the worst place to stop with an
// unexplained error.
func TestBackupKeepIsCheckedBeforeTheBackupRuns(t *testing.T) {
	for name, value := range map[string]string{
		"a word":     "lots",
		"a negative": "-1",
		"zero":       "0",
	} {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			env["STUB_BACKUP_ARCHIVE"] = archive
			env["BACKUP_KEEP"] = value

			r := run(t, env, "--no-pull", "--backup")

			if r.exit == 0 {
				t.Fatal("expected a refusal, got exit 0")
			}
			mustContain(t, r.stderr, "BACKUP_KEEP", "the refusal")
			mustNotContain(t, r.calls, "run --rm -T backup", "the calls")
			mustNotContain(t, r.calls, "migrate up", "the calls")
		})
	}
}

// Without --backup the value is never looked at, so a stale one in .env cannot
// refuse an update that is not taking a backup at all.
func TestBackupKeepIsIgnoredWithoutTheFlag(t *testing.T) {
	env := rollingEnv()
	env["BACKUP_KEEP"] = "lots"

	r := run(t, env, "--no-pull")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
}

// Spec 38 / decision D3: --auto takes the mode from `migrate plan`'s exit code.
// 3 is classic, 0 is rolling, and anything else aborts the run before anything
// is migrated - the codes are H14's, stubbed here.
func TestAutoTakesTheModeFromMigratePlan(t *testing.T) {
	cases := map[string]struct {
		rc      string
		classic bool
		abort   bool
	}{
		"0 is rolling":           {rc: "0", classic: false},
		"3 is classic":           {rc: "3", classic: true},
		"1 aborts":               {rc: "1", abort: true},
		"78 aborts":              {rc: "78", abort: true},
		"an unexpected 2 aborts": {rc: "2", abort: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			env["STUB_MIGRATE_PLAN_RC"] = c.rc

			r := run(t, env, "--no-pull", "--auto")

			mustContain(t, r.calls, "run --rm -T app migrate plan", "the calls")
			// The plan is asked after the build, which is what makes its answer
			// about the code that is about to be deployed.
			mustPrecede(t, r.calls, "build", "migrate plan", "the --auto sequence")

			if c.abort {
				if r.exit == 0 {
					t.Fatal("expected an abort, got exit 0")
				}
				mustContain(t, r.stderr, "could not plan the migration: migrate plan exited "+c.rc, "the abort")
				mustNotContain(t, r.calls, "migrate up", "the calls")
				mustNotContain(t, r.calls, "stop ts-inventory app", "the calls")
				mustNotContain(t, r.calls, "--scale app=2", "the calls")
				mustContain(t, lastLine(r.stdout), "phase=plan", "the summary")
				return
			}
			if r.exit != 0 {
				t.Fatalf("expected exit 0, got %d", r.exit)
			}
			if c.classic {
				mustContain(t, r.calls, "stop ts-inventory app", "the classic path")
				mustNotContain(t, r.calls, "--scale app=2", "the calls")
				mustContain(t, lastLine(r.stdout), "mode=classic", "the summary")
				return
			}
			mustContain(t, r.calls, "--scale app=2", "the rolling path")
			mustNotContain(t, r.calls, "stop ts-inventory app", "the calls")
			mustContain(t, lastLine(r.stdout), "mode=rolling", "the summary")
		})
	}
}

// Spec 38: "DEPLOY_MODE=classic forces classic; nothing forces rolling." The
// override is one-way and cannot be argued with, so the plan is not consulted
// at all - a plan that fails for its own reasons must not turn a deliberately
// careful deploy into a refusal.
func TestDeployModeClassicForcesClassicWithoutAsking(t *testing.T) {
	t.Run("over a plan that says rolling", func(t *testing.T) {
		env := rollingEnv()
		env["DEPLOY_MODE"] = "classic"
		env["STUB_MIGRATE_PLAN_RC"] = "0"

		r := run(t, env, "--no-pull", "--auto")

		if r.exit != 0 {
			t.Fatalf("expected exit 0, got %d", r.exit)
		}
		mustContain(t, r.calls, "stop ts-inventory app", "the classic path")
		mustNotContain(t, r.calls, "--scale app=2", "the calls")
		mustNotContain(t, r.calls, "migrate plan", "the calls")
		mustContain(t, lastLine(r.stdout), "mode=classic", "the summary")
	})

	// Nothing in the environment forces rolling: a plan that says classic is
	// obeyed however DEPLOY_MODE is set.
	t.Run("DEPLOY_MODE=rolling does not override a classic plan", func(t *testing.T) {
		env := rollingEnv()
		env["DEPLOY_MODE"] = "rolling"
		env["STUB_MIGRATE_PLAN_RC"] = "3"

		r := run(t, env, "--no-pull", "--auto")

		if r.exit != 0 {
			t.Fatalf("expected exit 0, got %d", r.exit)
		}
		mustContain(t, r.calls, "stop ts-inventory app", "the classic path")
		mustNotContain(t, r.calls, "--scale app=2", "the calls")
	})
}

// Without --auto the plan is never run: the operator's own --classic or the
// default rolling decides, exactly as before.
func TestNoPlanIsRunWithoutAuto(t *testing.T) {
	r := run(t, rollingEnv(), "--no-pull")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	mustNotContain(t, r.calls, "migrate plan", "the calls")
}

// Spec 38: the run's last line is one line the runner can put straight into the
// job summary, naming the mode taken, the archive written, the version now
// served and the phase reached. On a successful run the phase is the word after
// `update:` - `done`.
func TestSummaryLineEndsEverySuccessfulRun(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"rolling": {
			args: []string{"--no-pull"},
			want: "update: done mode=rolling ref=none version=dev backup=none",
		},
		"classic": {
			args: []string{"--no-pull", "--classic"},
			want: "update: done mode=classic ref=none version=dev backup=none",
		},
		"first start": {
			env: map[string]string{
				"STUB_PSQ_1":           "",
				"STUB_PSQ":             "",
				"STUB_APP_SERVICE_IDS": "",
				"STUB_APP_ALL_IDS":     "",
			},
			args: []string{"--no-pull"},
			want: "update: done mode=first-start ref=none version=dev backup=none",
		},
		"nothing to swap": {
			env:  map[string]string{"STUB_PLAN": " DRY-RUN MODE -  Container probe-app-1  Running"},
			args: []string{"--no-pull"},
			want: "update: done mode=nothing ref=none version=dev backup=none",
		},
		"the pipeline's own invocation": {
			env: map[string]string{
				"STUB_HEAD_1":         "1111111000000000000000000000000000000000",
				"STUB_HEAD_2":         "2222222000000000000000000000000000000000",
				"STUB_BACKUP_ARCHIVE": archive,
			},
			args: []string{"--ref", "v1.4.0", "--auto", "--backup"},
			want: "update: done mode=rolling ref=v1.4.0 version=v1.4.0 backup=" + archive,
		},
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
			if got := lastLine(r.stdout); got != c.want {
				t.Errorf("the summary line\n  got:  %q\n  want: %q", got, c.want)
			}
		})
	}
}

// Spec 38: "It is printed on failure too - a refusal says which phase it
// stopped in, which is what makes the table under Failure and rollback a lookup
// rather than a guess." Each case below is a row of that table in
// deploy/synology/README.md, "When a deploy fails", and the whole line is
// asserted rather than its phase alone: an operator reads `mode=` off the same
// line to pick the right half of the two rows more than one path reaches, so a
// wrong `mode=` would send them to the wrong row.
//
// `mode=unknown` is the value of every phase that runs before the path is
// chosen. It is a documented fifth value, not an accident: preflight, fetch,
// model, build and plan all run before the first-start branch, the
// nothing-to-do check and the classic/rolling split.
func TestSummaryLineNamesThePhaseAFailureStoppedIn(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"Compose is too old": {
			env:  map[string]string{"STUB_COMPOSE_VERSION": "2.20.1"},
			args: []string{"--no-pull"},
			want: "update: failed mode=unknown ref=none version=dev backup=none phase=preflight",
		},
		"a contradictory command line": {
			args: []string{"--no-pull", "--auto", "--classic"},
			want: "update: failed mode=unknown ref=none version=dev backup=none phase=preflight",
		},
		"the clone is dirty": {
			env:  map[string]string{"STUB_GIT_DIFF_RC": "1"},
			args: nil,
			want: "update: failed mode=unknown ref=none version=dev backup=none phase=preflight",
		},
		"the ref does not resolve": {
			env:  map[string]string{"STUB_GIT_REF_RC": "1"},
			args: []string{"--ref", "v9.9.9"},
			want: "update: failed mode=unknown ref=v9.9.9 version=dev backup=none phase=fetch",
		},
		"the merged model lists traefik": {
			env:  map[string]string{"STUB_SERVICES": "app db ts-inventory traefik"},
			args: []string{"--no-pull"},
			want: "update: failed mode=unknown ref=none version=dev backup=none phase=model",
		},
		// The build is the one step that fails without going through `die`, so
		// its phase would be unreachable without the stub's own build arm.
		"the build fails": {
			env:  map[string]string{"STUB_BUILD_RC": "1"},
			args: []string{"--no-pull"},
			want: "update: failed mode=unknown ref=none version=dev backup=none phase=build",
		},
		"the build fails under --ref": {
			env: map[string]string{
				"STUB_BUILD_RC": "1",
				"STUB_HEAD_1":   "1111111000000000000000000000000000000000",
				"STUB_HEAD_2":   "2222222000000000000000000000000000000000",
			},
			args: []string{"--ref", "v1.4.0"},
			// The version is already stamped by the time the build runs, so the
			// summary names the release that failed to build.
			want: "update: failed mode=unknown ref=v1.4.0 version=v1.4.0 backup=none phase=build",
		},
		"the plan cannot be read": {
			env:  map[string]string{"STUB_MIGRATE_PLAN_RC": "78"},
			args: []string{"--no-pull", "--auto"},
			want: "update: failed mode=unknown ref=none version=dev backup=none phase=plan",
		},
		"the backup fails": {
			env:  map[string]string{"STUB_BACKUP_RC": "1", "STUB_BACKUP_ARCHIVE": ""},
			args: []string{"--no-pull", "--backup"},
			want: "update: failed mode=rolling ref=none version=dev backup=none phase=backup",
		},
		// The classic path's own stop. Its recovery message deliberately does
		// not claim a stopped stack, which is why the phase has a row of its own.
		"the stop fails": {
			env:  map[string]string{"STUB_STOP_RC": "1"},
			args: []string{"--no-pull", "--classic"},
			want: "update: failed mode=classic ref=none version=dev backup=none phase=stop",
		},
		"the migration fails": {
			env:  map[string]string{"STUB_MIGRATE_RC": "1"},
			args: []string{"--no-pull", "--classic"},
			want: "update: failed mode=classic ref=none version=dev backup=none phase=migrate",
		},
		"the jobs will not drain": {
			env:  map[string]string{"STUB_PENDING": "3", "DRAIN_TIMEOUT": "0"},
			args: []string{"--no-pull"},
			want: "update: failed mode=rolling ref=none version=dev backup=none phase=drain",
		},
		"the new instance never becomes healthy": {
			env:  map[string]string{"STUB_HEALTH_RC": "1", "HEALTH_TIMEOUT": "0"},
			args: []string{"--no-pull"},
			want: "update: failed mode=rolling ref=none version=dev backup=none phase=start",
		},
		// phase=start is reached by three paths, and what is serving afterwards
		// differs for each - so `mode=` is what makes the table a lookup. These
		// two are the halves the rolling case above does not cover.
		"the classic path cannot start the stack again": {
			env:  map[string]string{"STUB_UP_RC": "1"},
			args: []string{"--no-pull", "--classic"},
			want: "update: failed mode=classic ref=none version=dev backup=none phase=start",
		},
		"the first start cannot start the stack": {
			env: map[string]string{
				"STUB_UP_RC":           "1",
				"STUB_PSQ_1":           "",
				"STUB_PSQ":             "",
				"STUB_APP_SERVICE_IDS": "",
				"STUB_APP_ALL_IDS":     "",
			},
			args: []string{"--no-pull"},
			want: "update: failed mode=first-start ref=none version=dev backup=none phase=start",
		},
		"the sidecar cannot be recreated": {
			env:  map[string]string{"STUB_SIDECAR_RC": "1"},
			args: []string{"--no-pull"},
			want: "update: failed mode=rolling ref=none version=dev backup=none phase=sidecar",
		},
		// The pipeline's own invocation, failing late: the summary still names
		// the release and the archive an operator would restore from.
		"a --ref --auto --backup run that fails at the drain": {
			env: map[string]string{
				"STUB_HEAD_1":         "1111111000000000000000000000000000000000",
				"STUB_HEAD_2":         "2222222000000000000000000000000000000000",
				"STUB_BACKUP_ARCHIVE": archive,
				"STUB_PENDING":        "2",
				"DRAIN_TIMEOUT":       "0",
			},
			args: []string{"--ref", "v1.4.0", "--auto", "--backup"},
			want: "update: failed mode=rolling ref=v1.4.0 version=v1.4.0 backup=" + archive + " phase=drain",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := rollingEnv()
			for k, v := range c.env {
				env[k] = v
			}

			r := run(t, env, c.args...)

			if r.exit == 0 {
				t.Fatal("expected a failure, got exit 0")
			}
			if got := lastLine(r.stdout); got != c.want {
				t.Errorf("the failure summary\n  got:  %q\n  want: %q", got, c.want)
			}
		})
	}
}

// --help is the one end that prints no summary: it is not a deploy at all. It
// prints the whole option list, which is read out of the script's own header.
func TestHelpPrintsEveryOptionAndNoSummary(t *testing.T) {
	r := run(t, rollingEnv(), "--help")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d", r.exit)
	}
	for _, opt := range []string{"--ref", "--auto", "--backup", "--classic", "--no-pull", "--force", "--prune"} {
		mustContain(t, r.stdout, opt, "the help text")
	}
	// The header documents the summary format, so those words do appear in the
	// help text; what must not appear is a summary line of its own at the end.
	if got := lastLine(r.stdout); strings.HasPrefix(got, "update: done ") || strings.HasPrefix(got, "update: failed ") {
		t.Errorf("--help printed a summary line: %q", got)
	}
	// It talks to nothing.
	if r.calls != "" {
		t.Errorf("--help still called something:\n%s", r.calls)
	}
}

// The two ways getting the code itself can fail. Both are refusals before the
// build, and both leave the clone where it was: what is deployed has to be
// what was fetched, so a fetch or a checkout that did not happen is not
// something to carry on from.
func TestRefRefusesWhenGitCannotGetTheCode(t *testing.T) {
	cases := map[string]struct {
		env    map[string]string
		expect string
		absent string
	}{
		"the fetch fails": {
			env:    map[string]string{"STUB_GIT_FETCH_RC": "1"},
			expect: "'git fetch origin --tags' failed",
			// Nothing was verified or checked out on the way out.
			absent: "git checkout",
		},
		"the checkout fails": {
			env:    map[string]string{"STUB_GIT_CHECKOUT_RC": "1"},
			expect: "could not check out 'v1.4.0'",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := refEnv()
			for k, v := range c.env {
				env[k] = v
			}

			r := run(t, env, "--ref", "v1.4.0")

			if r.exit == 0 {
				t.Fatal("expected a refusal, got exit 0")
			}
			mustContain(t, r.stderr, c.expect, "the refusal")
			mustContain(t, r.stderr, "Nothing was built or migrated", "the refusal")
			if c.absent != "" {
				mustNotContain(t, r.calls, c.absent, "the calls")
			}
			mustNotContain(t, r.calls, "build", "the calls")
			mustNotContain(t, r.calls, "migrate up", "the calls")
			mustContain(t, lastLine(r.stdout), "phase=fetch", "the summary")
		})
	}
}
