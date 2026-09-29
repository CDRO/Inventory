// entrypoint.sh's smoke_test() is the check that stands between a broken
// runner image and an operator discovering it during a release
// (docs/specs/38-release-pipeline-and-nas-runner.md, "The runner: a container
// on the NAS"). It is deliberately collect-rather-than-short-circuit: every
// failing check is reported, not just the first. None of its failure
// branches were exercised by anything before this file - the only thing that
// ran the script was the `runner-image` CI job, and that job only ever
// walks the happy path, on an image where everything is present and correct
// (issue #404).
//
// Same technique cmd/inventory/runner_compose_test.go's neighbour,
// deploy/synology/update_test.go, already establishes: stub docker-compose,
// docker and git on PATH, run the real script through /bin/sh (its shebang
// says #!/bin/bash, but it has no bashisms beyond `local`, which busybox ash
// also supports, and /bin/sh is what the dev image actually has - bash is
// not installed there), and assert on stdout/stderr and the exit code.
//
// The script hardcodes two absolute paths rather than reading them from the
// environment: RUNNER_HOME=/home/runner (it `cd`s there before the smoke
// test even runs) and the Docker socket /var/run/docker.sock (deliberately,
// per the script's own comment - a variable here could only ever disagree
// with the compose file's mount). Both are real filesystem side effects this
// suite makes in the container it runs in, not something a container or a
// real Docker daemon is needed for: /home/runner is created once, harmlessly,
// and the socket path gets a real AF_UNIX socket file for exactly as long as
// one test needs `[ -S ... ]` to be true, torn down via t.Cleanup. Neither
// test runs in parallel with another for that reason - both touch the same
// absolute paths.
package runner

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const dockerSockPath = "/var/run/docker.sock"

// The three stubs answer with a passing default for every check unless a
// scenario overrides one via STUB_*, and they log every invocation so the
// tests can be dropped in stdout for `t.Logf` even though nothing here reads
// the log back.
const composeStub = `#!/bin/sh
printf 'compose %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  *"version --short"*)
    echo "${STUB_COMPOSE_VERSION:-2.31.0}"; exit 0 ;;
  *"config -q"*)
    exit ${STUB_COMPOSE_CONFIG_RC:-0} ;;
esac
exit 0
`

const dockerStub = `#!/bin/sh
printf 'docker %s\n' "$*" >> "$STUB_LOG"
case "$1" in
  info)
    exit ${STUB_DOCKER_INFO_RC:-0} ;;
  version)
    echo "26.0.0"; exit 0 ;;
esac
exit 0
`

// Matched on "$*" rather than on $1/$2, because the clone path in the
// middle of `-C <clone> rev-parse ...` is a scenario's own temp directory and
// so cannot be matched as a literal positional argument.
const gitStub = `#!/bin/sh
printf 'git %s\n' "$*" >> "$STUB_LOG"
case "$*" in
  "--version")
    echo "git version 2.43.0"; exit 0 ;;
  *"rev-parse --git-dir"*)
    exit ${STUB_GIT_REVPARSE_RC:-0} ;;
  *"rev-parse --short HEAD"*)
    echo "abc1234"; exit 0 ;;
  *"ls-remote --exit-code origin HEAD"*)
    exit ${STUB_GIT_LSREMOTE_RC:-0} ;;
esac
exit 0
`

type result struct {
	exit   int
	stdout string
	stderr string
}

// scenario configures one run. stubs lists which of "docker-compose",
// "docker" and "git" are on PATH at all - a missing entry is how a scenario
// models a tool the image dropped, since the dev container this suite itself
// runs in never has any of the three installed for real. clone, left empty,
// gets a passingClone(t): both compose files present, nothing about a real
// .git directory, because check 4 ("is this a git checkout") is answered
// entirely by the git stub, not by anything on disk.
type scenario struct {
	stubs  []string
	clone  string
	env    map[string]string
	socket bool // stand in a real docker.sock so check 3 does not fail on "not mounted"
}

func defaultScenario() scenario {
	return scenario{stubs: []string{"docker-compose", "docker", "git"}, socket: true}
}

// passingClone is a clone directory the script accepts on disk: both compose
// files present. Whether $CLONE also looks like a git checkout is decided
// entirely by the git stub's STUB_GIT_REVPARSE_RC, not by anything here.
func passingClone(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"docker-compose.yml", "docker-compose.nas.yml"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func run(t *testing.T, sc scenario) result {
	t.Helper()

	script, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatalf("read the script under test: %v", err)
	}
	path := filepath.Join(t.TempDir(), "entrypoint.sh")
	if err := os.WriteFile(path, script, 0o755); err != nil {
		t.Fatal(err)
	}

	stubDir := t.TempDir()
	all := map[string]string{"docker-compose": composeStub, "docker": dockerStub, "git": gitStub}
	for _, name := range sc.stubs {
		body, ok := all[name]
		if !ok {
			t.Fatalf("unknown stub %q", name)
		}
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// RUNNER_HOME is hardcoded in the script (not an override), and it `cd`s
	// there under `set -eu` before the smoke test runs at all - a missing
	// directory would abort the whole script before a single check ran.
	if err := os.MkdirAll("/home/runner", 0o755); err != nil {
		t.Fatalf("create /home/runner (the script cds into it; the path is hardcoded, not an override): %v", err)
	}

	clone := sc.clone
	if clone == "" {
		clone = passingClone(t)
	}

	if sc.socket {
		if _, err := os.Stat(dockerSockPath); err == nil {
			t.Skip("a real docker.sock is already present at " + dockerSockPath + " - refusing to touch it")
		}
		l, err := net.Listen("unix", dockerSockPath)
		if err != nil {
			t.Fatalf("create a stand-in docker socket (the path the script checks is hardcoded, not an override): %v", err)
		}
		t.Cleanup(func() { l.Close() })
	}

	vars := map[string]string{
		"PATH":              stubDir + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"STUB_LOG":          filepath.Join(t.TempDir(), "calls.log"),
		"HOME":              t.TempDir(),
		"INVENTORY_CLONE":   clone,
		"RUNNER_STATE_DIR":  t.TempDir(),
		"RUNNER_WORK_DIR":   t.TempDir(),
		"RUNNER_SMOKE_ONLY": "1",
	}
	for k, v := range sc.env {
		vars[k] = v
	}

	cmd := exec.Command("/bin/sh", path)
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

	r := result{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
	t.Logf("exit %d\n--- stdout ---\n%s--- stderr ---\n%s", r.exit, r.stdout, r.stderr)
	return r
}

func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: expected to find %q in:\n%s", what, needle, haystack)
	}
}

// TestSmokeTestPassesOnACleanEnvironment is what makes every failure scenario
// below mean something: it proves the baseline scenario() actually clears
// all six checks, so a scenario that breaks exactly one thing can be trusted
// to fail for that reason and no other.
func TestSmokeTestPassesOnACleanEnvironment(t *testing.T) {
	r := run(t, defaultScenario())
	if r.exit != 0 {
		t.Fatalf("expected exit 0 on a clean environment, got %d\nstderr:\n%s", r.exit, r.stderr)
	}
	mustContain(t, r.stdout, "smoke test passed", "the ok summary")
	mustContain(t, r.stdout, "RUNNER_SMOKE_ONLY=1 - exiting without registering", "smoke-only stops before registration")
}

func TestDockerComposeMissingFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.stubs = []string{"docker", "git"} // no docker-compose on PATH
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "docker-compose does not run", "the missing-tool message")
}

func TestDockerComposeBelowVersionFloorFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.env = map[string]string{"STUB_COMPOSE_VERSION": "2.20.1"}
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "is below the 2.24", "the version-floor message")
}

func TestGitMissingFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.stubs = []string{"docker-compose", "docker"} // no git on PATH
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "git does not run", "the missing-tool message")
}

func TestDockerSocketNotMountedFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.socket = false // the natural state: no /var/run/docker.sock at all
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "is not a socket inside the container", "the not-mounted message")
}

func TestDockerSocketNotAnsweringFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.env = map[string]string{"STUB_DOCKER_INFO_RC": "1"}
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "daemon does not answer", "the not-answering message")
}

func TestCloneNotMountedFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.clone = filepath.Join(t.TempDir(), "does-not-exist")
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "is not mounted at", "the not-mounted message")
}

func TestCloneNotAGitCheckoutFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.env = map[string]string{"STUB_GIT_REVPARSE_RC": "1"}
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "exists but is not a git checkout", "the not-a-checkout message")
}

func TestComposeFilesMissingFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.clone = t.TempDir() // exists, but holds neither compose file
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "holds no docker-compose.yml", "the missing-files message")
}

func TestComposeFilesDoNotParseFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	sc.env = map[string]string{"STUB_COMPOSE_CONFIG_RC": "1"}
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "cannot parse", "the parse-failure message")
}

// TestGitRemoteUnreachableFailsTheSmokeTest covers the sixth check (#409,
// added after issue #404 was filed): it only runs when $CLONE/.git exists for
// real, which is the one check in this file the git stub cannot fully stand
// in for.
func TestGitRemoteUnreachableFailsTheSmokeTest(t *testing.T) {
	sc := defaultScenario()
	clone := passingClone(t)
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sc.clone = clone
	sc.env = map[string]string{"STUB_GIT_LSREMOTE_RC": "1"}
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "cannot reach origin", "the unreachable-remote message")
}

// TestMultipleFaultsAreAllReported pins the collect-rather-than-short-circuit
// behaviour the issue calls the part most likely to rot unnoticed: a refactor
// that turns a check into an early return still passes every scenario above
// on its own, and only shows up here.
func TestMultipleFaultsAreAllReported(t *testing.T) {
	sc := defaultScenario()
	sc.clone = t.TempDir() // exists, but holds neither compose file (check 5)
	sc.env = map[string]string{
		"STUB_COMPOSE_VERSION": "2.20.1", // check 1
		"STUB_GIT_REVPARSE_RC": "1",      // check 4
	}
	r := run(t, sc)
	if r.exit != 1 {
		t.Fatalf("expected exit 1, got %d", r.exit)
	}
	mustContain(t, r.stderr, "is below the 2.24", "check 1's fault, alongside the others")
	mustContain(t, r.stderr, "exists but is not a git checkout", "check 4's fault, alongside the others")
	mustContain(t, r.stderr, "holds no docker-compose.yml", "check 5's fault, alongside the others")

	// Untouched checks still pass and must not appear as faults.
	if strings.Contains(r.stderr, "git does not run") {
		t.Error("git was never broken in this scenario")
	}
	if strings.Contains(r.stderr, "daemon does not answer") {
		t.Error("the docker daemon was never broken in this scenario")
	}
}
