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
