package synology

// Tests for deploy/synology/boot, following the same approach as
// update_test.go (see its header comment for why: no host toolchain, so this
// is the only runner, and the script runs under the real busybox sh). It
// keeps its own stubs and its own run helper rather than update_test.go's,
// which are shaped around git and docker — boot touches neither, only
// docker-compose and sleep — but reuses result, mustContain, mustNotContain
// and projectSeq, which are generic.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The compose stub. version fails STUB_VERSION_FAILS times before it starts
// succeeding, which is what lets a scenario model the daemon not answering
// yet; config --services and up -d answer from the same STUB_* knobs the
// scenario sets, and up -d can be made to fail a fixed number of times the
// same way.
const bootComposeStub = `#!/bin/sh
printf 'compose %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  *"version"*)
    n=$(cat "$STUB_DIR/ver" 2>/dev/null || echo 0)
    n=$((n + 1)); echo "$n" > "$STUB_DIR/ver"
    [ "$n" -gt "${STUB_VERSION_FAILS:-0}" ] || exit 1
    echo "Docker Compose version v2.31.0"; exit 0 ;;
  *"config --services"*)
    [ -z "${STUB_SERVICES_RC:-}" ] || exit "${STUB_SERVICES_RC}"
    printf '%s\n' ${STUB_SERVICES:-app db ts-inventory}; exit 0 ;;
  *"up -d"*)
    n=$(cat "$STUB_DIR/up" 2>/dev/null || echo 0)
    n=$((n + 1)); echo "$n" > "$STUB_DIR/up"
    if [ "$n" -le "${STUB_UP_FAILS:-0}" ]; then
      echo "up -d attempt $n failed" >&2
      exit 1
    fi
    echo "Started"; exit 0 ;;
esac
exit 0
`

// sleep exits at once: the poll and retry loops count in their own variables,
// not the wall clock, so a stub that returns immediately costs no coverage —
// update_test.go's header explains why this matters inside `docker compose
// build`, which runs this suite.
const bootSleepStub = `#!/bin/sh
exit 0
`

func runBoot(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	return runBootIn(t, t.TempDir(), env, args...)
}

func runBootIn(t *testing.T, root string, env map[string]string, args ...string) result {
	t.Helper()

	script, err := os.ReadFile("boot")
	if err != nil {
		t.Fatalf("read the script under test: %v", err)
	}

	// Same layout update_test.go's runIn uses: the script locates its clone as
	// $(dirname $0)/../.., so the copy has to sit at that depth.
	dir := filepath.Join(root, "deploy", "synology")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "boot")
	if err := os.WriteFile(path, script, 0o755); err != nil {
		t.Fatal(err)
	}

	stubs := filepath.Join(root, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"docker-compose": bootComposeStub,
		"sleep":          bootSleepStub,
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	project := env["INVENTORY_PROJECT"]
	if project == "" {
		project = fmt.Sprintf("b%d", atomic.AddInt64(&projectSeq, 1))
	}
	log := filepath.Join(root, "calls.log")

	vars := map[string]string{
		"PATH":              stubs + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"STUB_LOG":          log,
		"STUB_DIR":          root,
		"INVENTORY_PROJECT": project,
		// The poll and retry loops' own sleep is stubbed out, so these only
		// bound how many times each loops before giving up.
		"BOOT_POLL":       "1",
		"BOOT_MAX_WAIT":   "3",
		"BOOT_RETRY_WAIT": "1",
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

func TestBootHappyPath(t *testing.T) {
	r := runBoot(t, nil)

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.exit, r.stderr)
	}
	mustContain(t, r.calls, "version", "the daemon probe")
	mustContain(t, r.calls, "config --services", "the model check")
	mustContain(t, r.calls, "up -d", "the start")
	mustContain(t, r.stdout, "done", "the final log line")
}

func TestBootNoWaitSkipsTheProbe(t *testing.T) {
	r := runBoot(t, nil, "--no-wait")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.exit, r.stderr)
	}
	mustNotContain(t, r.calls, "compose version", "with --no-wait")
}

func TestBootWaitsOutTheDaemon(t *testing.T) {
	// The probe fails twice, then answers: the script must retry rather than
	// giving up on the first failure.
	r := runBoot(t, map[string]string{"STUB_VERSION_FAILS": "2"})

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "answered after", "a probe that eventually succeeds")
}

func TestBootGivesUpWhenTheDaemonNeverAnswers(t *testing.T) {
	// BOOT_MAX_WAIT=3 and BOOT_POLL=1 (set by runBoot) bound this to a handful
	// of stubbed, instant iterations.
	r := runBoot(t, map[string]string{"STUB_VERSION_FAILS": "1000"})

	if r.exit == 0 {
		t.Fatalf("expected a non-zero exit, got 0")
	}
	mustContain(t, r.stdout, "never answered", "giving up on the daemon")
	mustNotContain(t, r.calls, "up -d", "a daemon that never answered")
}

func TestBootRetriesUpFailures(t *testing.T) {
	r := runBoot(t, map[string]string{"STUB_UP_FAILS": "2", "BOOT_RETRIES": "3"})

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.exit, r.stderr)
	}
	if n := strings.Count(r.calls, "up -d"); n != 3 {
		t.Errorf("expected 3 attempts at 'up -d', counted %d in:\n%s", n, r.calls)
	}
}

func TestBootStopsRetryingAfterTheLimit(t *testing.T) {
	r := runBoot(t, map[string]string{"STUB_UP_FAILS": "1000", "BOOT_RETRIES": "2"})

	if r.exit == 0 {
		t.Fatalf("expected a non-zero exit, got 0")
	}
	if n := strings.Count(r.calls, "up -d"); n != 2 {
		t.Errorf("expected exactly 2 attempts (BOOT_RETRIES=2), counted %d in:\n%s", n, r.calls)
	}
	mustContain(t, r.stdout, "failed after 2 attempt", "the give-up message")
}

func TestBootRefusesWhenTheModelListsTraefik(t *testing.T) {
	r := runBoot(t, map[string]string{"STUB_SERVICES": "app db ts-inventory traefik"})

	if r.exit == 0 {
		t.Fatalf("expected a non-zero exit, got 0")
	}
	mustContain(t, r.stdout, "traefik", "the NAS-layer guard")
	mustNotContain(t, r.calls, "up -d", "a model that lists traefik")
}

func TestBootRefusesWhenAServiceIsMissing(t *testing.T) {
	r := runBoot(t, map[string]string{"STUB_SERVICES": "app db"})

	if r.exit == 0 {
		t.Fatalf("expected a non-zero exit, got 0")
	}
	mustContain(t, r.stdout, "ts-inventory", "the missing-service guard")
	mustNotContain(t, r.calls, "up -d", "a model missing a required service")
}

func TestBootLogsToFile(t *testing.T) {
	root := t.TempDir()
	r := runBootIn(t, root, nil)

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.exit, r.stderr)
	}
	logged, err := os.ReadFile(filepath.Join(root, "boot.log"))
	if err != nil {
		t.Fatalf("expected ./boot.log in the clone: %v", err)
	}
	mustContain(t, string(logged), "done", "the on-disk log")
}

func TestBootHelpPrintsUsageAndNothingElse(t *testing.T) {
	r := runBoot(t, nil, "--help")

	if r.exit != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "BOOT_MAX_WAIT", "the printed usage")
	if r.calls != "" {
		t.Errorf("expected --help to call neither compose nor sleep, got:\n%s", r.calls)
	}
}

func TestBootRejectsAnUnknownOption(t *testing.T) {
	r := runBoot(t, nil, "--bogus")

	if r.exit == 0 {
		t.Fatalf("expected a non-zero exit, got 0")
	}
	mustContain(t, r.stderr, "unknown option", "the rejection")
}
