package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The `backup` service of docker-compose.yml is checked here rather than by
// running it, because the two things that matter about it are things a
// successful run would not reveal.
//
// A backup archives what is mounted into the container and nothing else. So
// "the archive contains nothing from imagecache" and "`.env` is absent"
// (docs/specs/15-backup-restore-and-export.md's first acceptance criterion)
// are not really claims about scripts/backup at all — they are claims about
// the mount list, and they hold because the bytes are not reachable from
// inside the container in the first place. A convenience mount added later
// would break both, silently, and every other test in the repository would
// stay green: the script would keep working, the archive would keep being
// written, and it would simply have started carrying a gigabyte of
// re-fetchable thumbnails and the operator's API keys.
//
// These assertions live in this package because a directory holding only test
// files is not a package `go build ./...` will accept, and the setup wizard's
// own test already reads the repository's real .env.example the same way
// (internal/config/setup_test.go).

// repoFile reads a file from the repository root.
func repoFile(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", name))
	require.NoError(t, err, "%s must exist at the repository root", name)
	return string(raw)
}

// composeService returns the configuration lines of one service of a compose
// file, with comments removed.
//
// Compose files here are hand-written with two-space service keys, so a block
// runs from `  <name>:` to the next line that starts a sibling at the same
// indent (or leaves the services map entirely). Text rather than a YAML parse
// on purpose: gopkg.in/yaml.v3 is currently an indirect dependency, and
// promoting it to a direct one to read four mount lines is a worse trade than
// this short reader.
//
// Comments are dropped because the assertions below are about what the service
// mounts, and these files explain themselves at length — a paragraph saying
// *why* imagecache is not mounted would otherwise fail the test asserting that
// it is not mounted.
func composeService(t *testing.T, file, name string) string {
	t.Helper()

	// Normalized first: this repository is checked out on Windows with
	// core.autocrlf=true, so every line here ends "\r\n" on the development
	// machine and "\n" in CI. Matching a line exactly is the whole mechanism
	// below, and without this the test would silently find no service at all
	// on one of the two.
	normalized := strings.ReplaceAll(repoFile(t, file), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	start := -1
	for i, line := range lines {
		if line == "  "+name+":" {
			start = i + 1
			break
		}
	}
	require.NotEqual(t, -1, start, "%s must declare a %q service", file, name)

	var block []string
	for _, line := range lines[start:] {
		if line != "" && !strings.HasPrefix(line, "   ") && !strings.HasPrefix(line, "  #") {
			// A sibling service, or the end of the map.
			break
		}
		if code, _, found := strings.Cut(line, " #"); found {
			line = strings.TrimRight(code, " ")
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		block = append(block, line)
	}
	return strings.Join(block, "\n")
}

// lineFrom returns the one line of block whose trimmed text starts with
// prefix, failing the test if there is none. Used to pull a value forward
// from one compose file's service block into an assertion on another's,
// rather than hardcoding what that value happened to be when the test was
// written - a literal copy would keep passing after the source drifted.
func lineFrom(t *testing.T, block, prefix string) string {
	t.Helper()

	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return line
		}
	}
	require.Fail(t, "no line found", "block has no line starting with %q", prefix)
	return ""
}

// TestBackupServiceMountsOnlyWhatMayTravel is the first acceptance criterion
// of docs/specs/15-backup-restore-and-export.md expressed as a test: a backup
// archive contains the SQL dump and the uploads tree, and nothing from
// imagecache; .env is absent.
func TestBackupServiceMountsOnlyWhatMayTravel(t *testing.T) {
	t.Parallel()

	block := composeService(t, "docker-compose.yml", "backup")

	assert.Contains(t, block, "image: postgres:16-alpine",
		"the same image as db, so pg_dump and BusyBox tar arrive with it and nothing is installed on the host")
	assert.Contains(t, block, `profiles: ["tools"]`,
		"a backup runs when it is invoked, never as part of `up`")

	assert.Contains(t, block, "- uploads:/data/uploads:ro",
		"the uploads tree is what a backup reads, and it reads it read-only")
	assert.Contains(t, block, "- uploads:/restore/uploads",
		"a restore needs one writable path into the same volume")
	assert.Contains(t, block, "- ./backups:/backups",
		"archives land in ./backups on the host")
	assert.Contains(t, block, "- ./scripts/backup:/usr/local/bin/inventory-backup:ro",
		"the script is mounted read-only rather than baked into an image")

	// The negative half, which is the half that fails silently.
	assert.NotContains(t, block, "imagecache",
		"the suggestion cache is re-fetchable by definition and must not be reachable from the backup container at all")
	assert.NotContains(t, block, "/data/cache",
		"the suggestion cache must not be reachable under its container path either")
	assert.NotContains(t, block, "- .:/work",
		"mounting the project directory would put .env — SESSION_SECRET and every API key — next to the archive source")
	assert.NotContains(t, block, "- ./.env",
		".env is the operator's to keep; an archive travels and secrets must not travel with it")
	assert.NotContains(t, block, "/var/run/docker.sock",
		"a backup container has no business holding the Docker socket")
	assert.NotContains(t, block, "depends_on",
		"`run` starts a service's dependencies, and \"the database is unreachable\" has to stay an outcome this command can report")
}

// TestNASVariantGivesBackupTheBindMountedUploads guards a failure that is
// invisible until the day someone needs the archive.
//
// docker-compose.nas.yml replaces the named volumes with bind mounts inside
// the clone, service by service. Compose merges `volumes` by container path,
// so a `backup` service that is only declared in the base file keeps pointing
// at the *named* volume `uploads` — which on the NAS holds nothing. Docker
// would create it on demand, empty; tar would archive an empty directory; the
// command would exit 0 and print a plausible archive path. Every image would
// be gone at restore time, and nothing before then would say so.
func TestNASVariantGivesBackupTheBindMountedUploads(t *testing.T) {
	t.Parallel()

	block := composeService(t, "docker-compose.nas.yml", "backup")

	assert.Contains(t, block, "- ./uploads:/data/uploads:ro",
		"on the NAS the real tree is the bind mount, not the named volume")
	assert.Contains(t, block, "- ./uploads:/restore/uploads",
		"a restore on the NAS must write into the same bind mount")
}

// TestE2EBackupServiceMatchesTheProductionOne guards a copy.
//
// The restore round trip (.github/workflows/restore.yml, issue #133) needs a
// `backup` service in the E2E stack, and docker-compose.e2e.yml carries its
// own rather than reusing the base file's, because that stack has no .env to
// read credentials from. Two declarations of one service is a thing that
// drifts, and the drift would be invisible: the round trip would keep passing
// against whatever the E2E copy had become, while the service it is meant to
// stand in for went on being something else.
//
// The mount list is the part worth pinning, for the reason
// TestBackupServiceMountsOnlyWhatMayTravel gives above — "nothing from
// imagecache, no .env" holds because those bytes are unreachable from inside
// the container, not because the script declines to read them.
func TestE2EBackupServiceMatchesTheProductionOne(t *testing.T) {
	t.Parallel()

	block := composeService(t, "docker-compose.e2e.yml", "backup")
	prodBlock := composeService(t, "docker-compose.yml", "backup")

	// Pulled from the production file itself, not hardcoded, so what this
	// test actually catches is the production service changing underneath
	// it: a literal copy of today's values would keep passing forever after
	// a real drift, which is the failure mode this test exists to prevent.
	for _, prefix := range []string{
		"image:",
		"entrypoint:",
		"profiles:",
		"- uploads:/data/uploads:ro",
		"- uploads:/restore/uploads",
		"- ./backups:/backups",
		"- ./scripts/backup:/usr/local/bin/inventory-backup:ro",
	} {
		assert.Contains(t, block, lineFrom(t, prodBlock, prefix),
			"the E2E backup service has drifted from the production one's %q line", prefix)
	}

	// The one intended difference from the base file's copy: no .env exists in
	// the E2E stack, so the credentials are inline — and they have to be the
	// ones `db` is started with, or the backup cannot authenticate at all.
	assert.Contains(t, block, "POSTGRES_USER: e2e",
		"the E2E stack has no .env, so the credentials are inline and must match db's")
	assert.Contains(t, block, "POSTGRES_DB: e2e",
		"the E2E stack has no .env, so the database name is inline and must match db's")
	assert.NotContains(t, block, "env_file",
		"there is no .env in the E2E stack to read; an env_file here would only ever be a missing file")

	// The same negative half as the production service, and it fails just as
	// silently here.
	assert.NotContains(t, block, "imagecache",
		"the suggestion cache is re-fetchable by definition and must not be reachable from the backup container")
	assert.NotContains(t, block, "/data/cache",
		"the suggestion cache must not be reachable under its container path either")
	assert.NotContains(t, block, "- .:/work",
		"mounting the project directory would put .env next to the archive source")
	assert.NotContains(t, block, "/var/run/docker.sock",
		"a backup container has no business holding the Docker socket")
	assert.NotContains(t, block, "depends_on",
		"`run` starts a service's dependencies, and the restore has to be able to report an unreachable database")
}

// TestE2EAppMountsTheUploadsTheRoundTripDestroys is the other half of that
// arrangement.
//
// The round trip's whole claim is that a `down -v` destroys the data and the
// archive brings it back. An uploads tree living in the app container's own
// writable layer would be recreated empty by the restart either way, so the
// round trip would "prove" the images came back without the archive having
// contributed anything. Only a named volume can actually be destroyed and
// actually be repopulated.
func TestE2EAppMountsTheUploadsTheRoundTripDestroys(t *testing.T) {
	t.Parallel()

	block := composeService(t, "docker-compose.e2e.yml", "app")

	assert.Contains(t, block, "- uploads:/data/uploads",
		"the uploads tree must be a named volume, or `down -v` destroys nothing and the restore proves nothing")
	assert.NotContains(t, block, "- uploads:/data/uploads:ro",
		"the app writes new uploads, so its mount must not be read-only - Contains alone would also accept this line")
	assert.NotContains(t, block, "imagecache",
		"imagecache is not in a backup archive, so the E2E stack has no reason to hold one")
}

// TestE2EComposeFilePinsItsProjectName — the E2E stack is one Compose project
// per machine, `inventory-e2e` (#190, and that file's own `name:` comment).
// Locally every documented command passes `-p inventory-e2e`, because a
// checkout's COMPOSE_PROJECT_NAME beats a file's `name:`. CI passes no `-p` at
// all: a runner has no `.env`, so this one line is the whole of the separation
// there, across every invocation in .github/workflows/e2e.yml and
// .github/workflows/restore.yml (H8 split the round trip into the latter).
//
// Which makes it the rare line whose removal breaks nothing visibly. Delete or
// rename it and every suite stays green — no Go code reads it, neither
// PowerShell suite loads the file, and CI's own E2E job still passes, because a
// fresh runner has no dev stack for the E2E project to collide with. What it
// reopens is #190's original failure on a developer machine: this file would
// share the default `inventory` project with docker-compose.yml, and `db` would
// come up on the dev stack's own container and data directory instead of a
// disposable one. That was measured, not theorized — in a checkout where the
// two shared a project, `psql -U e2e` answered `role "e2e" does not exist`
// because the data directory had already been initialized under the dev role.
//
// The companion fact — that the documented sequence in that file's header
// carries `-p inventory-e2e` — is deliberately not asserted here. It lives in
// prose that is meant to be reworded, and composeService strips comments for
// exactly that reason. This is a value, so it can be pinned.
func TestE2EComposeFilePinsItsProjectName(t *testing.T) {
	t.Parallel()

	normalized := strings.ReplaceAll(repoFile(t, "docker-compose.e2e.yml"), "\r\n", "\n")

	// An exact line match, the way composeService finds a service key: column 0
	// is what distinguishes the project name from a `name:` nested inside some
	// service, and every other mention of `inventory-e2e` in this file is
	// inside a `#` comment, which a substring match would happily accept.
	// Scanned rather than asserted with Contains over the whole file so that a
	// failure prints the message below instead of two hundred lines of YAML.
	pinned := false
	for _, line := range strings.Split(normalized, "\n") {
		if line == "name: inventory-e2e" {
			pinned = true
			break
		}
	}

	assert.True(t, pinned,
		"docker-compose.e2e.yml must pin the project name at the top level: CI passes no -p, so without this line the E2E stack shares the default project with docker-compose.yml and db reuses the dev stack's data directory (#190)")
}

// The three tests below are decision D5 of docs/plans/2026-09-harness-
// optimization.md expressed as tests: throwaway databases (the dev override,
// the E2E stack) trade crash durability for write speed, and production
// (docker-compose.yml, docker-compose.nas.yml) does not. The negative
// assertion on docker-compose.yml is the half that fails silently — a flag
// added there by mistake would not break a single test in this repository
// and would only ever be noticed by a crash on the operator's NAS losing
// data it was supposed to keep.

// TestProductionDatabaseKeepsDurabilityDefaults guards docker-compose.yml and
// docker-compose.nas.yml: the base file's `db` service must carry no `command:`
// at all, so Postgres runs on its own durability defaults, and the NAS overlay
// must not add one either.
func TestProductionDatabaseKeepsDurabilityDefaults(t *testing.T) {
	t.Parallel()

	prodBlock := composeService(t, "docker-compose.yml", "db")
	assert.NotContains(t, prodBlock, "command:",
		"production's db service must run Postgres on its own defaults - a command: line here would trade a NAS operator's crash durability for speed nobody asked for")
	for _, flag := range []string{"fsync=off", "synchronous_commit=off", "full_page_writes=off"} {
		assert.NotContains(t, prodBlock, flag,
			"production's db service must not carry the throwaway-database speed flag %q", flag)
	}

	nasBlock := composeService(t, "docker-compose.nas.yml", "db")
	assert.NotContains(t, nasBlock, "command:",
		"the NAS overlay must not add a command: to db either - it only replaces the data directory with a bind mount")
}

// TestDevOverrideDatabaseTradesDurabilityForSpeed is decision D5's dev-loop
// half: docker-compose.override.yml's db service carries the three speed
// flags, merged onto docker-compose.yml's db (image, env, healthcheck and the
// persistent pgdata volume all still come from there - see the override
// file's own comment on this service).
func TestDevOverrideDatabaseTradesDurabilityForSpeed(t *testing.T) {
	t.Parallel()

	block := composeService(t, "docker-compose.override.yml", "db")
	assert.Contains(t, block, "command: postgres -c fsync=off -c synchronous_commit=off -c full_page_writes=off",
		"the dev override's db service must set all three speed flags in one command:, or Compose's scalar-replace semantics mean only the last one written survives")
}

// TestE2EDatabaseMatchesTheOverrideDatabaseSpeedFlags is decision D5's E2E
// half, pulled forward from the override's own service rather than hardcoded
// so what this test actually catches is the two drifting apart - a literal
// copy of today's flags would keep passing after either file changed under it.
func TestE2EDatabaseMatchesTheOverrideDatabaseSpeedFlags(t *testing.T) {
	t.Parallel()

	overrideBlock := composeService(t, "docker-compose.override.yml", "db")
	e2eBlock := composeService(t, "docker-compose.e2e.yml", "db")

	assert.Contains(t, e2eBlock, lineFrom(t, overrideBlock, "command:"),
		"the E2E stack's db service has drifted from the dev override's own speed flags")
}

// TestCIComposeAddsTmpfsToDatabaseOnly guards the new docker-compose.ci.yml:
// override-style, and its only content is a tmpfs data directory on db, never
// a service this package does not own.
func TestCIComposeAddsTmpfsToDatabaseOnly(t *testing.T) {
	t.Parallel()

	block := composeService(t, "docker-compose.ci.yml", "db")
	assert.Contains(t, block, "target: /var/lib/postgresql/data",
		"docker-compose.ci.yml must replace the data directory at the same mount target docker-compose.yml's pgdata volume uses, or Compose's merge-by-target-path would keep both")
	assert.Contains(t, block, "type: tmpfs",
		"a CI runner's whole VM is destroyed at the end of the job, so the data directory belongs in memory, not on the runner's disk")

	// Enumerated rather than a single NotContains("\n  app:"): that caught an
	// `app:` service specifically but let a `traefik:`, `setup:` or `backup:`
	// block through unnoticed (#327/#333 item 4). Not hypothetical: wave 2's
	// H8 already wired this file into .github/workflows/test.yml's live
	// COMPOSE_FILE, so it is no longer an inert file nobody edits under
	// pressure. Same text-scan approach as TestE2EComposeFilePinsItsProjectName:
	// a two-space-indented `key:` line is a top-level service, a
	// more-indented line is that service's own content, and anything at
	// column 0 (or blank/a comment) ends or is outside the services map.
	normalized := strings.ReplaceAll(repoFile(t, "docker-compose.ci.yml"), "\r\n", "\n")
	var services []string
	inServices := false
	for _, line := range strings.Split(normalized, "\n") {
		switch {
		case line == "services:":
			inServices = true
		case !inServices, line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, "   "):
			// not yet in the map, a blank/comment line, or a service's own
			// nested content (3+ leading spaces) - none of these are a
			// top-level service key.
		case !strings.HasPrefix(line, "  "):
			// dedented back to column 0: the services map has ended.
			inServices = false
		default:
			name, _, found := strings.Cut(strings.TrimPrefix(line, "  "), ":")
			require.True(t, found, "docker-compose.ci.yml: %q under services: is not a %q key", line, "key:")
			services = append(services, name)
		}
	}
	assert.Equal(t, []string{"db"}, services,
		"docker-compose.ci.yml is scoped to db only - any other service here would belong to a different package's file")
}

// TestBackupArchivesAreNotCommittable — an archive holds the whole database
// and every photo in it. The service writes them into the clone, so the only
// thing standing between a backup and the repository is this line.
func TestBackupArchivesAreNotCommittable(t *testing.T) {
	t.Parallel()

	assert.Contains(t, repoFile(t, ".gitignore"), "/backups/",
		"a backup archive must never be committable")
	assert.Contains(t, repoFile(t, ".dockerignore"), "backups",
		"and must never be sent to the daemon as build context")
}

// The two assertions below are static reads of scripts/backup rather than runs
// of it.
//
// Executing the script from `go test` is genuinely out of reach: the test
// container has no pg_dump, no psql and no second database to drop. The
// behaviours are nonetheless the two that fail catastrophically and quietly —
// an archive that looks complete but is truncated, and a restore that proceeds
// into a live stack — so reading the script for the shape of each is worth more
// than leaving both to a manual run that happened once. A static check cannot
// prove the script works; it can prove nobody removed the part that makes it
// safe, which is the regression actually worth catching.

// TestBackupWritesUnderATemporaryNameAndRenames — a cron-driven backup that
// dies half way through must leave nothing that looks like a backup, because
// the next disaster is when anyone would find out. rename(2) within one
// directory is atomic, so the final name never names a partial file.
func TestBackupWritesUnderATemporaryNameAndRenames(t *testing.T) {
	t.Parallel()

	script := repoFile(t, "scripts/backup")

	assert.Contains(t, script, `partial="$BACKUP_DIR/.inventory-backup-$stamp.tar.gz.part"`,
		"the archive is built under a temporary name")
	assert.Contains(t, script, `tar -czf "$partial"`,
		"tar writes to the temporary name, never straight to the final one")
	assert.Contains(t, script, `mv "$partial" "$final"`,
		"the final name appears only once the archive is complete")
	assert.Contains(t, script, `--no-owner --no-privileges`,
		"the dump must name no database role, or a restore onto new hardware needs the old one")
}

// TestRestoreRefusesAgainstALiveDatabase — a restore into a running stack
// corrupts both ends, and this is the check that actually protects the DROP.
// It is asked of pg_stat_activity rather than of Docker because a container
// with no socket has no other way to ask that a network hiccup could not answer
// wrongly in the permissive direction.
func TestRestoreRefusesAgainstALiveDatabase(t *testing.T) {
	t.Parallel()

	script := repoFile(t, "scripts/backup")

	assert.Contains(t, script, "pg_stat_activity",
		"the live-stack guard asks the database who else is connected")
	assert.Contains(t, script, "pg_backend_pid()",
		"the guard must not count its own connection")
	assert.Contains(t, script, "refusing to restore:",
		"and must refuse rather than proceed")
	assert.Contains(t, script, "DELETE FROM sessions;",
		"a restore invalidates every session of the backed-up instance: a session id is "+
			"an opaque row, signed with nothing, so reloading the dump would otherwise "+
			"hand back working cookies from before the disaster")
}

// TestBackupScriptIsPinnedToLFEndings — the script is bind-mounted into the
// container straight from the checkout, so the checkout's line endings are the
// ones the BusyBox shell reads. This repository is developed on Windows with
// core.autocrlf=true, which would otherwise turn the shebang into "#!/bin/sh\r"
// and the script would simply stop running.
func TestBackupScriptIsPinnedToLFEndings(t *testing.T) {
	t.Parallel()

	assert.Contains(t, repoFile(t, ".gitattributes"), "scripts/backup text eol=lf")
	assert.NotContains(t, repoFile(t, "scripts/backup"), "\r",
		"the working copy of the script must have LF endings, whatever the platform checked it out")
}
