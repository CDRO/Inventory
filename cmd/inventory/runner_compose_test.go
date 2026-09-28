package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The self-hosted runner (deploy/synology/runner/,
// docs/specs/38-release-pipeline-and-nas-runner.md) is checked here for the same
// reason the `backup` service above is: everything that matters about it is
// something a successful run would not reveal.
//
// A runner that mounts one directory too many still starts, still registers,
// still reports itself idle and still deploys correctly. So does a runner whose
// clone is mounted at a different path inside than out — right up to the first
// release, which then binds the wrong host directories into the app stack
// because those paths are resolved by the HOST's daemon. And a runner that
// registered as `--ephemeral`, or under a per-container name, works perfectly
// once and then needs a human and a fresh token. None of that is visible in a
// green build, in the image's own smoke test, or in a successful deploy.
//
// These are therefore assertions about text, deliberately: the file IS the
// security control ("the runner mounts nothing beyond the clone and the socket"
// is spec 38's stated containment, not an implementation detail), and the thing
// to protect it from is a later edit that looks reasonable.
//
// It lives beside compose_test.go, and uses its composeService/lineFrom
// readers, because it is the same kind of claim about the same kind of file.

const runnerCompose = "deploy/synology/runner/docker-compose.runner.yml"

// scriptCode reads a shell script or Dockerfile from the repository with its
// comments removed, so that an assertion about what the file DOES is not
// satisfied - or, worse, broken - by a paragraph explaining it. Both files here
// explain themselves at length, and the entrypoint's longest comment is the one
// naming `--ephemeral` as the option it deliberately does not use.
//
// Same two mechanics as composeService above, for the same two reasons: CRLF is
// normalised because the repository is checked out on Windows with
// core.autocrlf=true, and a trailing comment is cut at " #" rather than at "#"
// so that shell parameter expansions (`${v#*.}`) survive.
func scriptCode(t *testing.T, name string) string {
	t.Helper()

	var code []string
	for _, line := range strings.Split(strings.ReplaceAll(repoFile(t, name), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if kept, _, found := strings.Cut(line, " #"); found {
			line = strings.TrimRight(kept, " ")
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// envDefault returns the default of a compose `NAME: ${NAME:-default}` line in
// block — the value, not the syntax.
//
// The distinction is the whole reason this exists rather than a `Contains` for
// `"${NAME:-"`: that prefix is a prefix of `${NAME:-}` too, so a check written
// that way accepts an emptied default, which for several of these variables is
// precisely the regression the setting is there to prevent. Returning the
// default lets the caller assert something about it.
func envDefault(t *testing.T, block, name string) string {
	t.Helper()

	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lineFrom(t, block, name+":")), name+":"))
	prefix, suffix := "${"+name+":-", "}"
	require.True(t, strings.HasPrefix(value, prefix) && strings.HasSuffix(value, suffix),
		"%s must be written as an override with a default, ${%s:-<default>}, so the NAS needs no variable set and another value needs no edit to this file; got %q",
		name, name, value)
	return strings.TrimSuffix(strings.TrimPrefix(value, prefix), suffix)
}

// dockerfileEnv returns the value the Dockerfile's ENV assigns to name.
//
// The ENV is one instruction spanning several backslash-continued lines, so the
// line for one variable ends in " \" for every variable but the last; both forms
// are accepted rather than assumed.
func dockerfileEnv(t *testing.T, dockerfile, name string) string {
	t.Helper()

	raw := strings.TrimSpace(lineFrom(t, dockerfile, name+"="))
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, name+"="), `\`))
	require.NotEmpty(t, value, "the Dockerfile's ENV must give %s a value", name)
	return value
}

// TestRunnerMountsOnlyTheSocketTheCloneAndItsState is spec 38's "Mounts, and
// nothing else" as a test.
//
// The runner holds the Docker socket, so it is root on the host whatever else it
// mounts — that is accepted and documented. What the mount list buys is not
// privilege but REACH: after this repository goes private, any workflow on any
// branch can name `runs-on: self-hosted` and land here (runner groups are a paid
// feature), and then the only thing standing between that workflow and a
// directory on the NAS is whether this file mounts it.
func TestRunnerMountsOnlyTheSocketTheCloneAndItsState(t *testing.T) {
	t.Parallel()

	block := composeService(t, runnerCompose, "runner")

	var mounts []string
	for _, line := range strings.Split(block, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "- ") {
			mounts = append(mounts, trimmed)
		}
	}

	// Counted, not just spot-checked: a `Contains` assertion per expected mount
	// would pass just as happily with a fourth one added underneath.
	require.Len(t, mounts, 3,
		"the runner mounts exactly three things; a fourth is reach a workflow that lands here would inherit: %v", mounts)

	assert.Contains(t, mounts, "- /var/run/docker.sock:/var/run/docker.sock",
		"the host's Docker daemon is what the runner is for")
	assert.Contains(t, mounts, "- ./runner_state:/runner/state",
		"the registration lives in a bind mount so a DSM reboot needs no token and no human")
	// The clone's own line is asserted in full by the next test.

	assert.NotContains(t, block, "pgdata",
		"the live database is reachable through the socket anyway, but nothing should mount it by name")
	assert.NotContains(t, block, "- ./.env",
		"the runner reads the deployment's .env through the clone, never as its own environment")
	assert.NotContains(t, block, "- .:/",
		"mounting the project root a second time, at a second path, is exactly the silent wrong-path bug")
	assert.NotContains(t, block, "ports:",
		"nothing ever connects TO the runner: it polls GitHub outbound and talks to the socket")
	assert.NotContains(t, block, "privileged",
		"socket access is already root on the host; a privileged flag would add nothing but noise to the risk statement")
}

// TestRunnerCloneIsMountedAtTheIdenticalPath guards the one property of this
// file that fails silently and expensively.
//
// docker-compose.nas.yml's bind mounts (./pgdata, ./uploads, ./imagecache,
// ./backups) are relative to the clone, and a container the runner starts
// through the socket has those paths resolved by the HOST's daemon, not by the
// runner's filesystem. Mount the clone at /work instead, and the deploy still
// succeeds — against ./pgdata, ./uploads and ./backups directories that Compose
// helpfully creates, empty, under /work on the host. The app would come up with
// no data and no images, and nothing before that moment would have complained.
//
// Both halves of the mount and the env var the smoke test reads are therefore
// asserted to be the same interpolation, rather than each being asserted against
// a hardcoded path: a test that hardcodes /volume1/docker/inventory keeps passing
// after someone moves the clone and edits only one of the three places.
func TestRunnerCloneIsMountedAtTheIdenticalPath(t *testing.T) {
	t.Parallel()

	block := composeService(t, runnerCompose, "runner")

	envLine := strings.TrimSpace(lineFrom(t, block, "INVENTORY_CLONE:"))
	clone := strings.TrimSpace(strings.TrimPrefix(envLine, "INVENTORY_CLONE:"))
	require.True(t, strings.HasPrefix(clone, "${INVENTORY_CLONE:-") && strings.HasSuffix(clone, "}"),
		"INVENTORY_CLONE must be an override with a default, so the NAS needs no variable and another path needs no edit: %q", clone)
	assert.Contains(t, clone, "/volume1/docker/inventory",
		"the default is the clone path docs/specs/01-architecture-and-deployment.md documents for the NAS")

	assert.Equal(t, "- "+clone+":"+clone, strings.TrimSpace(lineFrom(t, block, "- ${INVENTORY_CLONE")),
		"source and target must be the identical string, from the identical variable — the paths inside the runner are resolved by the host's daemon")
}

// TestRunnerIsItsOwnComposeProjectThatSurvivesAReboot covers the two settings
// that decide what happens when a human is not watching.
//
// The project name is in the file rather than in every command because a
// forgotten -p would land the runner in a project named after its directory,
// next to the app stack it exists to recreate; `dc down` on the app stack would
// then stop the thing doing the deploying, mid-deploy. `restart: unless-stopped`
// plus the state directory is the whole of "a DSM reboot brings the runner back
// with no token and no human" (decision D4).
func TestRunnerIsItsOwnComposeProjectThatSurvivesAReboot(t *testing.T) {
	t.Parallel()

	file := strings.ReplaceAll(repoFile(t, runnerCompose), "\r\n", "\n")
	assert.Contains(t, file, "\nname: inventory-runner\n",
		"its own Compose project, declared in the file, so that `dc down` on the app stack never stops the runner")

	block := composeService(t, runnerCompose, "runner")
	assert.Contains(t, block, "restart: unless-stopped",
		"a DSM reboot must bring the runner back on its own")
	assert.Contains(t, block, "container_name: inventory-runner",
		"exactly one runner by design, and the removal procedure in the README is a `docker exec` at this name")

	// Not a hostname, not omitted: --replace replaces the registration OF THE
	// SAME NAME, so a name that changes per container registers a new runner on
	// every recreate and leaves the old ones behind as offline duplicates.
	//
	// The DEFAULT is what is asserted, not the presence of the override syntax.
	// A `Contains` check for the `${RUNNER_NAME:-` prefix passes just as happily
	// against `${RUNNER_NAME:-}`, which is the one value that reintroduces the
	// bug this setting exists to prevent — an empty default is exactly how
	// config.sh ends up back at the container hostname.
	//
	// Equality against the image's own ENV rather than against the literal
	// "nas-inventory": the invariant is "non-empty, and the same in both places
	// it is written down". Renaming the runner deliberately means editing both,
	// which is the point; renaming it in one place is the drift this catches.
	name := envDefault(t, block, "RUNNER_NAME")
	require.NotEmpty(t, name,
		"an empty default is the regression: config.sh falls back to the container hostname, Docker regenerates it on every recreate, and --replace then reclaims nothing and registers a duplicate")
	assert.Equal(t, dockerfileEnv(t, scriptCode(t, "deploy/synology/runner/Dockerfile"), "RUNNER_NAME"), name,
		"the compose default and the image's own ENV default must not drift apart")
}

// TestRunnerRegistersPersistentlyAsRoot is decision D4, which spec 38 quotes
// verbatim precisely because the compose file, the release workflow and the
// operator runbook all have to agree about it.
func TestRunnerRegistersPersistentlyAsRoot(t *testing.T) {
	t.Parallel()

	entrypoint := scriptCode(t, "deploy/synology/runner/entrypoint.sh")

	assert.Contains(t, entrypoint, "--replace",
		"re-registering the same name must reclaim the existing runner, not add an offline duplicate")
	assert.Contains(t, entrypoint, "--unattended",
		"a missing answer must be an error, not a prompt nobody is there to read")
	assert.NotContains(t, entrypoint, "--ephemeral",
		"--ephemeral de-registers after one job and so needs a fresh token per job, which means a classic PAT with repo scope living on the NAS permanently")

	dockerfile := scriptCode(t, "deploy/synology/runner/Dockerfile")

	assert.Contains(t, dockerfile, "RUNNER_LABELS=self-hosted,nas,synology",
		"exactly these three labels; release.yml selects the runner with two of them")
	assert.Contains(t, dockerfile, "RUNNER_ALLOW_RUNASROOT=1",
		"root in the container is honest about what socket access already is; see the README's residual-risk paragraph")
	assert.Contains(t, dockerfile, "USER root",
		"DSM's socket is root-owned with no group to hand over, and a chgrp is undone by the next DSM update")
}

// TestRunnerImagePinsWhatItInstalls keeps the image reproducible and its
// downloads verified.
//
// The runner is built by hand on the NAS a handful of times a year, so "it built
// last time" is the only evidence anyone has. An unpinned base or an unverified
// download turns that into "it built last time, from whatever was newest then".
func TestRunnerImagePinsWhatItInstalls(t *testing.T) {
	t.Parallel()

	dockerfile := scriptCode(t, "deploy/synology/runner/Dockerfile")

	assert.Contains(t, dockerfile, "FROM ghcr.io/actions/actions-runner:",
		"GitHub's own runner image, because the agent inside it self-updates and a hand-built one would not")
	assert.Contains(t, dockerfile, "@sha256:",
		"the base image is pinned by digest, not only by a tag that can be moved")
	assert.Contains(t, dockerfile, "sha256sum -c -",
		"the docker-compose binary is verified against a published checksum before it is made executable")
	assert.Contains(t, dockerfile, "ARG COMPOSE_VERSION=v2.31.0",
		"the same standalone Compose docs/specs/01-architecture-and-deployment.md installs on the NAS, so the runner and a hand-typed command behave identically")

	// The three tools deploy/synology/update needs. Two come with the base
	// image, which is why the Dockerfile asserts them: a base that drops one
	// must fail this build, in CI, rather than a release on the NAS.
	//
	// Asserted against the assertion's own loop line, not against the whole
	// file: "docker" occurs in `docker-compose`, in `COMPOSE_VERSION` and in
	// half the comments, so a whole-file Contains would stay green with the
	// tool check deleted outright. Reading the line rather than matching it
	// literally keeps it indifferent to the order of the three names.
	toolCheck := lineFrom(t, dockerfile, "for t in ")
	for _, tool := range []string{"git", "docker", "curl"} {
		assert.Contains(t, toolCheck, tool,
			"the build must assert %s, which deploy/synology/update requires", tool)
	}
	assert.Contains(t, dockerfile, "the base image no longer ships",
		"the tool check must fail the build loudly rather than leave the image short of a tool")
}

// TestRunnerStateNeverTravels checks the two ignore files.
//
// ./runner_state holds .credentials — the key this NAS authenticates to GitHub
// Actions with — and it sits inside the clone, which is both a git checkout and
// the build context of the app image (`COPY . .`). Without both entries the
// credential is one `git add -A` or one `docker compose build` away from leaving
// the machine, and in the second case it would leave inside an image layer,
// where nobody would think to look for it.
func TestRunnerStateNeverTravels(t *testing.T) {
	t.Parallel()

	assert.Contains(t, repoFile(t, ".gitignore"), "/deploy/synology/runner/runner_state/",
		"the runner's registration must never be committed")
	assert.Contains(t, repoFile(t, ".dockerignore"), "deploy/synology/runner/runner_state",
		"the runner's registration must never reach an image layer of the app")

	// Full path, not a bare name: .dockerignore's existing patterns are
	// root-anchored by design (its own comment says so), so `runner_state`
	// alone would match nothing.
	assert.NotContains(t, repoFile(t, ".dockerignore"), "\nrunner_state",
		"a bare pattern would match nothing here — .dockerignore patterns are root-anchored")
}
