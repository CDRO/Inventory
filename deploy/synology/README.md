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

1. `git pull --ff-only`, check the pulled compose files, pull the sidecar image,
   build the app image.
2. `migrate up` — the *old* app is still serving while this runs.
3. Wait until **no job is pending** (up to `DRAIN_TIMEOUT`, 60 s).
4. Start a **second** app instance from the new image, next to the old one.
5. Wait until the new instance answers `GET /healthz` (up to `HEALTH_TIMEOUT`,
   120 s).
6. Wait **once more** until no job is pending, then stop and remove the old
   instance. The count is back to one.
7. Recreate the Tailscale sidecar.

Before it pulls anything it refuses to run if more than one app instance is
already running, or if a stopped app container is left over (an earlier update was
interrupted): it names what to remove with `docker rm -f`.

If the update stops before step 6 completes, the old instance is **untouched**: a
failed build or a failed migration changes nothing; a new instance that fails to
start, does not become healthy, or is interrupted (Ctrl-C, a dropped SSH session)
is removed again, and the script says so. Two things do stay: **the database is
already migrated** from step 2 on (forward-only, see below), and a job the old
instance was running keeps running. From step 6 on the new app is already serving.

| Option | Effect |
|---|---|
| `--no-pull` | Skip `git pull` (you pulled already, or you are on a branch without upstream). |
| `--classic` | Stop app and sidecar, migrate, start everything. See below. |
| `--force` | Swap even if Compose sees nothing to change. |
| `--prune` | Afterwards, remove dangling images. |
| `-h`, `--help` | Print the header of the script. |

| Environment | Meaning |
|---|---|
| `TS_AUTHKEY` | Passed through to the sidecar's very first start (see "First start"). |
| `HEALTH_TIMEOUT`, `DRAIN_TIMEOUT` | Seconds; defaults 120 and 60. |
| `DOCKER_COMPOSE` | The compose command (default `docker-compose`). Set it to the full path of the standalone binary where `PATH` does not have it. |
| `INVENTORY_PROJECT` | The project name (default `inventory`). For testing; leave it alone on the NAS. |

**Nothing to do.** After the build, the script asks Compose (`up --dry-run`) whether
it would recreate, create or start anything. If not — same image, same
configuration, and the sidecar image is the one that runs — it says so and exits
without touching anything. A newer sidecar image or a changed compose file counts
as a change, and the update goes ahead. (Docker Desktop's containerd image store
writes a new image id at every build, so there this check never finds "nothing";
the NAS's Docker does not.)

**One at a time.** A lock directory (`/tmp/inventory-update-inventory.lock`) keeps a
second run out. A run that was killed hard leaves it behind; the message names it.

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
- **The app assumes one process, and the script can only narrow the gap.** On
  start the app marks *every* pending job as failed (`FailInterruptedJobs`), and on
  a normal stop it cancels the jobs it is running and fails them as interrupted. The
  script waits for no job to be pending before it starts the second instance
  (step 3) and again before it stops the old one (step 6). A job submitted in the
  seconds after a check can still be failed: by the new instance's start-up, or by
  the old instance's stop. The user re-runs the analysis. Closing that needs the
  recovery to know which process owns a job (issue #121). `--classic` avoids the
  overlap altogether, at the price of interrupting whatever is running.
- **A crash of the app is not handled by any of this.** Docker restarts the app
  container by itself, and the sidecar keeps the dead namespace until you run
  `dc restart ts-inventory`.

### Going back

Migrations only go forward (spec 18): there is no `migrate down`. **Going back to an
earlier release after a migration means restoring the backup you took before the
update.** Checking out an old commit and rebuilding starts an old binary on a newer
schema, which spec 18 makes a fatal start-up error.

If the schema did not change between the two releases, the code alone can go back:

```console
$ git checkout <commit>                       # detached HEAD
$ sh deploy/synology/update --no-pull --force
$ git switch main                             # later: so that the next update can pull
```

### Verifying a change to these scripts

There is no automated test for them; a change is verified against a scratch stack,
never against the real one. In a scratch clone (`git clone` of the branch, a dummy
`.env`, and `INVENTORY_PROJECT=scratch DOCKER_COMPOSE="docker compose"`, so the
real project `inventory` and its containers are not touched), run and check:

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
| A second app container started by hand | The script refuses before it pulls. |
| The lock directory created by hand | The script refuses and names it. |
| `--classic` with a failing migration | Stack stays stopped and the message says so. |

Clean up with `dc down -v` in the scratch clone and delete it.
