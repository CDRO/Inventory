# Synology NAS scripts

How-to for the scripts in this folder. The design rules they follow are in
[`docs/specs/01-architecture-and-deployment.md`](../../docs/specs/01-architecture-and-deployment.md),
section "Synology NAS variant". Everything here is run **on the NAS, in a root
shell** (`sudo -i`), and uses `docker-compose` with the hyphen — the standalone
Compose ≥ 2.24 from the spec, not the older one bundled with Container Manager.

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
- `--remove` takes the block out again. `--profile FILE` writes somewhere other
  than `~/.profile`.
- It refuses a clone path that contains whitespace, because `$DC` cannot hold it.
- Only login shells read `~/.profile`. Task Scheduler jobs do not: call
  `sh /volume1/docker/inventory/deploy/synology/update` there by full path.
- A plain `sudo <command>` may not read root's profile; use `sudo -i`.

Without the script, set the same thing by hand, once per session, in the clone's
folder: `DC="docker-compose -p inventory -f docker-compose.yml -f docker-compose.nas.yml"`.

## `update` — the safe update

```console
$ sh deploy/synology/update
```

The default is a **rolling update**. The old app keeps serving until the new one
has proved itself:

1. `git pull --ff-only`, pull the sidecar image, build the app image.
2. `migrate up` — the *old* app is still serving while this runs.
3. Wait until **no job is pending** (up to `DRAIN_TIMEOUT`, 60 s).
4. Start a **second** app instance from the new image, next to the old one.
5. Wait until the new instance answers `GET /healthz` (up to `HEALTH_TIMEOUT`,
   120 s).
6. Stop and remove the old instance. The count is back to one.
7. Recreate the Tailscale sidecar.

If the update stops before step 6, the old instance is **untouched**: a failed
build or migration changes nothing; a new instance that does not become healthy
is removed again. The script prints its last log lines and says so. From step 6
on the new app is already serving.

| Option | Effect |
|---|---|
| `--no-pull` | Skip `git pull` (you pulled already, or you are on a branch without upstream). |
| `--classic` | Stop app and sidecar, migrate, start everything. See below. |
| `--force` | Swap even if the image is unchanged. |
| `--prune` | Afterwards, remove dangling images. |
| `-h`, `--help` | Print the header of the script. |

| Environment | Meaning |
|---|---|
| `TS_AUTHKEY` | Passed through to the very first start of the sidecar (one-off key; see the spec). |
| `HEALTH_TIMEOUT`, `DRAIN_TIMEOUT` | Seconds; defaults 120 and 60. |
| `DOCKER_COMPOSE`, `INVENTORY_PROJECT` | For testing: the compose command (default `docker-compose`) and the project name (default `inventory`). Leave them alone on the NAS. |

**Nothing to do.** If the image just built has the same layers as the one the app
runs, the script says so and exits without touching anything. The running image is
kept under the tag `inventory-app:running` for that comparison.

**First start.** When no app instance is running, the script builds, migrates and
starts the whole stack — the sequence of the spec's "First start", after
`$DC run --rm setup`.

**Refuses to run** unless the merged model lists `app`, `db` and `ts-inventory`
and does not list `traefik` (that means the NAS layer is not active), and unless
Compose is at least 2.24. It also refuses if more than one app instance is
already running (an earlier update was interrupted): keep the one you want, remove
the others with `docker rm -f <id>`, and run it again.

### What the rolling update cannot do

- **It is not zero-downtime.** The Tailscale sidecar shares the app's network
  namespace, so it must be recreated when the app container changes. Expect a few
  seconds without the tailnet URL at step 7. What the rolling update buys you is
  that a release that does not start never replaces one that does.
- **Two releases overlap.** The migration of step 2 runs against the old app, and
  the old app runs on the migrated schema until step 6. A migration that the
  previous release cannot work with needs `--classic`.
- **The app assumes one process.** On start it marks *every* pending job as failed
  (`FailInterruptedJobs`, "no goroutine can be working on anything yet"). Step 3
  waits for the pending jobs to finish, which covers the usual case; a job
  submitted in the few seconds between that check and the new instance's start is
  failed while the old instance may still be working on it. The user re-runs the
  analysis. `--classic` avoids the overlap altogether, at the price of
  interrupting whatever is running.
- **A crash of the app is not handled by any of this.** Docker restarts the app
  container by itself, and the sidecar keeps the dead namespace until you run
  `dc restart ts-inventory`.

### Going back to an earlier release

```console
$ git checkout <commit>
$ sh deploy/synology/update --no-pull --force
```

Migrations are not undone; go back only to a release the current schema can serve.
