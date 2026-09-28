# Synology NAS scripts

How-to for the scripts in this folder. The design rules they follow are in
[`docs/specs/01-architecture-and-deployment.md`](../../docs/specs/01-architecture-and-deployment.md),
section "Synology NAS variant"; the upgrade rules they sit inside (back up first,
migrations only go forward) are in
[`docs/specs/18-operations-and-observability.md`](../../docs/specs/18-operations-and-observability.md),
"Upgrades". Everything here is run **on the NAS, in a root shell** (`sudo -i`), and
uses `docker-compose` with the hyphen — the standalone Compose ≥ 2.24 from the
spec, not the older one bundled with Container Manager.

| File | What it is |
|---|---|
| `install-shell` | Puts `$DC` and the alias `dc` into your login shell, from any directory. |
| `update` | Updates the stack without replacing a working app by a broken one. |
| `compose` | Optional shorthand: the compose prefix as a script. Nothing depends on it. |
| `tailscale/serve.json` | The `tailscale serve` rule of the sidecar. |

All three scripts must keep their executable bit and **LF** line endings
(`.gitattributes` enforces that for the repository; a copy made by hand from a
Windows checkout has CRLF and will not run). If one will not run, start it as
`sh deploy/synology/<name>`; that fixes a missing executable bit, not CRLF.

**The app container is no longer called `inventory_app`.** The fixed name had to go
so that two instances can run side by side. Compose names them `inventory-app-<n>`;
use `dc ps` (or `docker ps`) to see the current one, and `dc exec app …`,
`dc logs app` instead of `docker exec inventory_app`. The first `update` on a stack
that was started with the old name replaces that container like any other.

## `install-shell` — `$DC` and `dc` everywhere

```console
$ sh deploy/synology/install-shell
Installed in /root/.profile for the clone at /volume1/docker/inventory.
Log in again, or run:  . /root/.profile
$ dc ps
```

It writes one marked block into `~/.profile` (root's, when you run it from
`sudo -i` or an SSH login as root):

```sh
# >>> inventory NAS shell (deploy/synology/install-shell) >>>
export INVENTORY_DIR="/volume1/docker/inventory"
export DC="docker-compose -p inventory -f $INVENTORY_DIR/docker-compose.yml -f $INVENTORY_DIR/docker-compose.nas.yml"
alias dc="$DC"
# <<< inventory NAS shell <<<
```

- `$DC` is **exported**, so scripts and subshells see it; `dc` is the same as an
  alias for interactive use. Both carry **both** `-f` files, so the dev override
  is never merged in, and they use **absolute paths**, so they work from any
  directory.
- Idempotent: running it again replaces its own block and touches nothing else in
  the file. Run it again after you move the clone.
- Before it changes a profile it copies it to `<profile>.inventory-bak`. It
  **refuses** a profile whose markers are unbalanced (a start line without its end
  line, or the reverse) and changes nothing; fix or delete the marker lines by hand.
- It refuses a clone path with anything but letters, digits and `. _ / + -`: the
  path lands in a double-quoted profile line that the shell expands at every login.
- `--remove` takes the block out again (and does nothing, creating no file, when
  there is none). `--profile FILE` writes somewhere other than `~/.profile`.
- Only login shells read `~/.profile`. A Task Scheduler job does not: see below.
- A plain `sudo <command>` may not read root's profile; use `sudo -i`.

Without the script, set the same thing by hand, once per session, in the clone's
folder: `DC="docker-compose -p inventory -f docker-compose.yml -f docker-compose.nas.yml"`.

## `update` — the safe update

**Back up first.** The upgrade rules in spec 18 apply: take a backup
([`15-backup-restore-and-export.md`](../../docs/specs/15-backup-restore-and-export.md))
before you update. The script does not, and it cannot undo a migration.

```console
$ sh deploy/synology/update
```

The default is a **rolling update**. The old app keeps serving until the new one
has proved itself:

1. Refuse if the clone has local changes to tracked files, then `git pull --ff-only`,
   check the pulled compose files, pull the sidecar image, build the app image.
2. `migrate up` — the *old* app is still serving while this runs.
3. Wait until **no job is pending** (up to `DRAIN_TIMEOUT`, 60 s). If jobs are
   still pending when that runs out, or the count cannot be read at all, the
   update **aborts here** — it does not wait and then carry on. The database is
   already migrated by then; try again later, or use `--classic`.
4. Start a **second** app instance from the new image, next to the old one.
5. Wait until the new instance answers `GET /healthz` (up to `HEALTH_TIMEOUT`,
   120 s).
6. Wait **once more** until no job is pending — aborting again, on the same two
   conditions — then stop and remove the old instance. The count is back to one.
7. Recreate the Tailscale sidecar.

Before it pulls anything it refuses to run if more than one app instance is
already running, or if a stopped app container is left over (an earlier update was
interrupted): it names what to remove with `docker rm -f`.

If the update stops before step 6 completes, the old instance is **untouched**: a
new instance that fails to start, does not become healthy, or is interrupted
(Ctrl-C, a dropped SSH session) is removed again, and the script says so. What the
*database* is left as depends on where it stopped:

- **A failed `git pull` or a failed build changes nothing at all.** Step 2 never
  ran, so the schema is still the one the old app has been serving on.
- **A failed migration can leave the release half-applied.** goose applies the
  pending migrations one at a time, each in its own transaction, and stops at the
  one that failed — the ones before it stay applied. The schema then sits between
  the two releases, and the old app keeps serving against it.
- **Anything after step 2 leaves the database fully migrated**: an aborted drain,
  an unhealthy new instance, an error, Ctrl-C. Migrations only go forward (see
  "Going back"), so the way out is forward.

A job the old instance was running also keeps running. From step 6 on the new app
is already serving — and if the run ends between step 6 and step 7, the sidecar
still holds the retired instance's network namespace, so the tailnet URL is down
until it is recreated. The script prints that one command when it exits that way.

| Option | Effect |
|---|---|
| `--no-pull` | Skip `git pull` and the clean-clone check: deploy the tree as it is (you pulled already, or you are on a branch without upstream). |
| `--classic` | Stop app and sidecar, migrate, start everything. See below. |
| `--force` | Swap even if Compose sees nothing to change. |
| `--prune` | Afterwards, remove dangling images. |
| `--ref <tag\|sha>` | Deploy that ref instead of pulling: fetch, verify, check out detached, and build it as `VERSION=<tag>` (or the short sha). Refuses `--no-pull`. |
| `--auto` | Let `migrate plan` choose between the rolling and the classic path. Refuses `--classic`. |
| `--backup` | Archive the instance into `./backups` before migrating; a failed backup aborts the update. |
| `-h`, `--help` | Print the header of the script. |

| Environment | Meaning |
|---|---|
| `TS_AUTHKEY` | Passed through to the sidecar's very first start (see "First start"). |
| `HEALTH_TIMEOUT`, `DRAIN_TIMEOUT` | Seconds; defaults 120 and 60. |
| `DOCKER_COMPOSE` | The compose command (default `docker-compose`). Set it to the full path of the standalone binary where `PATH` does not have it. |
| `INVENTORY_PROJECT` | The project name (default `inventory`). For testing; leave it alone on the NAS. |
| `DEPLOY_MODE` | `classic` forces the classic path under `--auto` (the annotated tag's `deploy: classic` line). Nothing forces rolling. |
| `BACKUP_KEEP` | How many `--backup` archives to keep in `./backups`; read from the environment, then from `.env`, then 5. |

**Nothing to do.** After the build, the script asks Compose (`up --dry-run`) whether
it would recreate, create or start anything. If not — same image, same
configuration, and the sidecar image is the one that runs — it says so and exits
without touching anything. A newer sidecar image or a changed compose file counts
as a change, and the update goes ahead. (Docker Desktop's containerd image store
writes a new image id at every build, so there this check never finds "nothing";
the NAS's Docker does not.)

**One at a time.** A lock directory (`/tmp/inventory-update-inventory.lock` — the
last part is `INVENTORY_PROJECT`, and the path does not depend on `TMPDIR`, so an
interactive run and a scheduled one share the same lock) keeps a second run out. It
holds the pid and start time of the run that took it. A run killed hard (SIGKILL, a
power cut) leaves the directory behind, and the next run then says whether that pid
is still alive. Removing it is left to you on purpose: a run killed that hard can
also have left a second app instance behind, which wants the same look.

**First start.** When no app instance is running, the script builds, migrates and
starts the whole stack — the sequence of the spec's "First start", after
`$DC run --rm setup` has written `.env`. Pass the one-off Tailscale key on the
command line the first time, or the sidecar starts without joining the tailnet and
the script still exits 0:

```console
$ TS_AUTHKEY=tskey-auth-… sh deploy/synology/update --no-pull
```

**Refuses to run** unless the merged model lists `app`, `db` and `ts-inventory`
and does not list `traefik` (the NAS layer is not active), and unless Compose is at
least 2.24.

### Deploying a release: `--ref`, `--auto`, `--backup`

The release pipeline
([`38-release-pipeline-and-nas-runner.md`](../../docs/specs/38-release-pipeline-and-nas-runner.md))
runs one command on the NAS, and reads nothing but its exit code and its last
line:

```console
$ sh deploy/synology/update --ref v1.4.0 --auto --backup
```

**`--ref <tag|sha>`** replaces the pull. It fetches `origin` with its tags,
verifies the ref resolves, checks it out **detached**, and stamps the build with
it: `VERSION=<tag>` for a tag, the short sha for anything else, which
`docker-compose.yml` passes into the image and `GET /healthz` reports back. What
is deployed is then one named commit instead of wherever a branch happened to
point. The clean-clone refusal applies exactly as it does to the pull, and a ref
that does not resolve is a refusal *before* anything is built or migrated. Since
`--ref` is how the run gets its code, `--ref` and `--no-pull` together are an
error. A clone left detached this way keeps deploying by tag; `git switch main`
puts it back on the branch for a plain `update`.

**`--auto`** takes the choice between rolling and classic away from the
operator. After the build it runs `inventory migrate plan`, which reads the
`-- +inventory:classic` marker of the pending migrations:

| `migrate plan` exits | What `--auto` does |
|---|---|
| `0` | rolling — no pending migration the previous release cannot serve, or nothing pending at all |
| `3` | classic — a pending migration carries the marker |
| anything else | aborts before anything is migrated: `could not plan the migration: migrate plan exited <n>` |

`DEPLOY_MODE=classic` in the environment forces the classic path without
consulting the plan at all — that is how an annotated tag whose message carries
`deploy: classic` reaches this script. **Nothing forces rolling**: the override
is one-way, and an operator who believes a marker is wrong edits the migration
and cuts a new tag. `--auto` and `--classic` together are an error.

**`--backup`** runs the `backup` service
([`15-backup-restore-and-export.md`](../../docs/specs/15-backup-restore-and-export.md))
into `./backups` **before the schema is touched**, on every path that migrates —
and, on the classic path, before anything is stopped, so that a failed backup
leaves the old release serving. A backup that fails, or that exits 0 without
writing an archive, **aborts the update with nothing migrated**: migrations only
go forward, so that archive is the only way back across one. Afterwards the
newest `BACKUP_KEEP` archives are kept (default 5; from the environment, else
from `.env`) and the older ones are deleted. A half-written
`.inventory-backup-<stamp>.tar.gz.part` is never deleted — it belongs to a backup
that is still running — and neither is anything else you keep in `./backups`.

The `backup` service has no `depends_on`, on purpose (a backup that quietly
starts the database it was meant to dump is how a failing backup goes
unnoticed), so on a **first start** with nothing running at all it has no
database to reach. Start the stack without `--backup` that one time.

**The summary line.** The last line of every run that got past its own command
line says what happened, in one line:

```
update: done mode=rolling ref=v1.4.0 version=v1.4.0 backup=inventory-backup-2026-09-28-0300.tar.gz
update: failed mode=rolling ref=v1.4.0 version=v1.4.0 backup=none phase=drain
```

`mode` is `rolling`, `classic`, `first-start` or `nothing`. On success the word
after `update:` is the phase reached (`done`); on failure `phase=` names the one
it stopped in — the lookup key of the table below. It goes to stdout either way;
the recovery messages keep going to stderr. The exit code is `0` on success
**and** on nothing-to-do, `1` on every refusal and abort, and the signal exits
(`130`, `143`, `129`) are unchanged.

### When a deploy fails

A failed deploy is a **stop**, not a rollback: in most rows below the previous
release is still serving, which is the point of the rolling design. Read
`phase=` off the summary line and look it up:

| `phase=` | What is serving | What the database is | The way through |
|---|---|---|---|
| `preflight` | the old release, untouched | untouched | Fix what it named: Compose < 2.24, the lock, two app instances or a leftover stopped one, a dirty clone, a bad `BACKUP_KEEP`. |
| `fetch` | the old release, untouched | untouched | The ref does not resolve, or `git fetch` failed. Nothing was built. |
| `model` | the old release, untouched | untouched — but the clone now stands at the new ref | The merged model is not the NAS one. The message names the commit the clone moved to. |
| `build` | the old release, untouched | untouched | Fix the build. |
| `plan` | the old release, untouched | untouched | `migrate plan` could not answer. Nothing was migrated. |
| `backup` | the old release, untouched | untouched | The archive was not taken, so the run stopped before `migrate up`. |
| `migrate` | the old release (rolling); stopped (classic) | possibly **partially** migrated: goose applies each migration in its own transaction and stops at the one that failed | Forward-only. Fix the migration and run again, or restore the archive. |
| `drain` | the old release | migrated | Jobs would not drain, or the count could not be read. Re-tag with `deploy: classic`, or deploy by hand with `--classic`. |
| `start` | the old release; the new instance is removed again | migrated | The new instance never became healthy. Its last 30 log lines are on stderr. |
| `retire`, `sidecar` | the **new** release, but the tailnet URL is down until the sidecar is recreated | migrated | Run the `--force-recreate ts-inventory` command the exit trap printed. |

**Rollback is restoring the pre-upgrade archive** ("Going back" below;
[`15-backup-restore-and-export.md`](../../docs/specs/15-backup-restore-and-export.md)
has the procedure) — that is what `--backup` takes it for, and the summary line
names it. Going back across a release that migrated **nothing** needs no restore
at all: deploy the older tag, and
`git diff --stat <older tag> <tag> -- migrations/` printing nothing is how you
know that is the case.

### Deploying by hand

When the runner is down, or before there is one, the same command from `sudo -i`
in the clone is the whole procedure — it is also what this script always was:

```console
$ cd /volume1/docker/inventory
$ sh deploy/synology/update --ref v1.4.0 --auto --backup
```

Leave `--ref` off to deploy the branch tip (`git pull --ff-only`), and `--auto`
off to choose the path yourself with `--classic` or the default rolling.

### From Task Scheduler

A scheduled task does not read `~/.profile`, so `docker-compose` (the standalone
binary, for example in `/volume1/docker/bin`) is not on its `PATH`. Give the task
this command, with your paths:

```console
DOCKER_COMPOSE=/volume1/docker/bin/docker-compose sh /volume1/docker/inventory/deploy/synology/update
```

### What the rolling update cannot do

- **It is not zero-downtime.** The Tailscale sidecar shares the app's network
  namespace, so it must be recreated when the app container changes. Expect a few
  seconds without the tailnet URL at step 7. What the rolling update buys you is
  that a release that does not start never replaces one that does.
- **For a short time both instances receive requests.** While the new instance
  starts, the name `app` resolves to both containers, and the sidecar proxies to
  `http://app:8000`. A request can reach the new instance before its health check
  has passed.
- **Two releases overlap.** The migration of step 2 runs against the old app, and
  the old app runs on the migrated schema until step 6. A migration that the
  previous release cannot work with needs `--classic`.
- **Starting the new instance no longer fails the old one's jobs; stopping the old
  one still can.** A pending job records which process is working it and start-up
  fails only the jobs whose owner is gone, so step 4 leaves the old instance's work
  alone. On a normal stop, though, the app still cancels the jobs it is running and
  fails them as interrupted — it is not racing anything, it is saying it will not
  finish them. The script waits for no job to be pending before it starts the second
  instance (step 3) and again before it stops the old one (step 6); a job submitted
  in the seconds after that second check is failed by the stop, and the user re-runs
  the analysis. `--classic` avoids the overlap altogether, at the price of
  interrupting whatever is running.
- **A new instance the trap removes no longer strands its jobs.** It is force-removed
  with `docker rm -f`, so the jobs it had accepted stay `pending` with no process
  behind them. The surviving instance fails them within about a minute — it sweeps
  claims nobody renews for as long as it serves, not only at start-up — so the next
  update's drain wait is not blocked by them.
- **A crash of the app is not handled by any of this.** Docker restarts the app
  container by itself, and the sidecar keeps the dead namespace until you run
  `dc restart ts-inventory`.

### Going back

Migrations only go forward (spec 18): there is no `migrate down`. **Going back to an
earlier release after a migration means restoring the backup you took before the
update.** Checking out an old commit and rebuilding starts an old binary on a newer
schema, which spec 18 makes a fatal start-up error.

If the schema did not change between the two releases, the code alone can go back.
Check that first — the question is whether anything was added under `migrations/`:

```console
$ git diff --stat <commit> HEAD -- migrations/   # no output: no schema change
$ git checkout <commit>                       # detached HEAD
$ sh deploy/synology/update --no-pull --force
$ git switch main                             # later: so that the next update can pull
```

If that diff prints anything, the schema did change and this recipe is not
available: the old binary refuses to start against the newer schema (`migrate.Check`
runs at every start, per spec 18) and you are back to restoring the backup.

### Verifying a change to these scripts

`docker compose run --rm app go test ./deploy/...`
([`update_test.go`](update_test.go)) covers the parts of `update` that are
*decisions* rather than Docker: it runs the real script under busybox `sh` against
recording stubs for `docker`, `docker-compose` and `git`, and asserts which
commands it issued, with which container ids, and what it printed. That catches a
wrong branch, a wrong message, a wrong id and a missing abort. It proves nothing
about the daemon, about Compose, or about the DSM — the stubs only reproduce what
Compose 2.31.0 was observed to do.

So a change is still verified against a scratch stack as well, never against the
real one. In a scratch clone (`git clone` of the branch, a dummy `.env`, and
`INVENTORY_PROJECT=scratch DOCKER_COMPOSE="docker compose"`, so the real project
`inventory` and its containers are not touched), run and check:

| Scenario | Expected |
|---|---|
| Nothing running | Builds, migrates, starts app, db and sidecar. |
| A commit that changes an embedded file (for example a comment in `web/static/index.html`), then `update` | `git pull` advances; the old app container is gone, the new one serves the change, the sidecar's network mode is `container:<new app id>`. |
| `update` again with nothing new | On the NAS's Docker: "nothing to swap". |
| `--force` several times in a row | Each exits 0 and leaves exactly one app container. |
| `HEALTH_TIMEOUT=0 update --force` | The new instance is removed; the old app and the sidecar keep their ids. |
| A `ports:` entry added to `app` in the clone's `docker-compose.nas.yml`, then `update --force` | The second instance cannot start (host port taken); no stopped app container is left; the old app is untouched. |
| A pending job (`INSERT INTO jobs …`), `DRAIN_TIMEOUT=4` | Aborts, nothing swapped; after the job is `done`, the update goes through. |
| A broken migration file in the clone | The build succeeds, `migrate up` fails, the old app and the sidecar are untouched. |
| A `RUN false` in the `Dockerfile` | The build fails; nothing changed. |
| A local change to a tracked file, then `update` | Refuses and lists the file; with `--no-pull` it deploys the tree as it is. |
| A second app container started by hand | The script refuses before it pulls. |
| The lock directory created by hand | The script refuses, names it, and says whether the pid inside it is still alive. |
| `--classic` with a failing migration | Stack stays stopped and the message says so, with the `up -d` that starts it again. |
| `--prune` on each of the four ends: first start, "nothing to swap", `--classic`, rolling | A dangling image the build left behind is gone in all four cases. |
| `--ref` to a tag that exists (`git tag -a v0.0.1-scratch -m x`) | Fetches, detaches onto it, prints `<old> -> <new>`, and the built image reports that version; the summary line says `ref=v0.0.1-scratch version=v0.0.1-scratch`. |
| `--ref` to a ref that does not exist | Refuses after the fetch and before the build; nothing is built or migrated, and the summary says `phase=fetch`. |
| `--backup` on a running stack | An archive appears in `./backups`, the summary names it, and it was written before `migrate up` ran. |
| `--backup` with the database stopped (`dc stop db`) | The backup fails, the run aborts with "Nothing was migrated", no `migrate up` ran, and on `--classic` nothing was stopped either. |
| `--backup` with six archives already in `./backups` and a `.part` beside them | The newest five survive, the older ones are gone, and the `.part` is untouched. |
| `--auto` on a scratch clone | Runs `migrate plan` after the build and takes the path its exit code names; an exit code that is neither `0` nor `3` aborts before anything is migrated. `DEPLOY_MODE=classic … --auto` takes the classic path without running the plan at all. |
| `traefik` in the merged model (drop the `-f docker-compose.nas.yml`) | Refuses; no container is touched, and after a pull that moved the clone the message names the commit it moved to. |
| Compose older than 2.24 (`DOCKER_COMPOSE=` the Container Manager binary) | Refuses before it touches anything. |
| `kill` (SIGTERM) of the run between the health check and step 6 | Exit 143; the new instance is removed, the old app and the sidecar keep their ids, and the lock directory is gone. |
| `dc run --rm app migrate status` left running while an update aborts | The trap removes the new app instance and leaves that one-off container alone. |
| `install-shell`, then `install-shell` again, then `--remove` | One marked block in `~/.profile`; `dc` is an alias; the backup matches the original; `--remove` takes the block out and creates no file when there is none; an unbalanced marker leaves the profile byte-identical. |

Clean up with `dc down -v` in the scratch clone and delete it.
