# The self-hosted GitHub Actions runner

This folder is the runner that deploys this project onto the NAS: a container
that polls GitHub for jobs labelled `self-hosted, nas, synology` and runs

```console
$ sh deploy/synology/update --ref <tag> --auto --backup
```

in the clone, on the host's own Docker daemon. The rules it implements are
[`docs/specs/38-release-pipeline-and-nas-runner.md`](../../../docs/specs/38-release-pipeline-and-nas-runner.md)
("The runner: a container on the NAS", "Registration and privilege",
"Security posture"); this file is the how-to.

Everything below is run **on the NAS, in a root shell** (`sudo -i`) with the
standalone `docker-compose` on `PATH`, exactly like the rest of
[`deploy/synology/README.md`](../README.md). The two commands that are **not**
run on the NAS — minting the registration and removal tokens — are run on your
own machine, and say so.

| File | What it is |
|---|---|
| `Dockerfile` | GitHub's `actions/runner`, pinned by digest, plus the pinned standalone `docker-compose` and the entrypoint below. |
| `entrypoint.sh` | The start-up smoke test, then register-once, then the listener. |
| `docker-compose.runner.yml` | Its own Compose project, `inventory-runner`. Never merged with the app stack's files. |
| `runner_state/` | Created on first start; holds the registration. Gitignored — it is a credential. |

## What this container can do

**It is root on the NAS.** It mounts `/var/run/docker.sock`, and a process that
can talk to the Docker socket can start a privileged container, bind-mount `/`
and read or write anything on the host — `.env`, `pgdata`, your backups, DSM's
own configuration. No uid, capability drop or read-only mount inside the
container changes that, which is why it does not pretend to: it runs as root with
`RUNNER_ALLOW_RUNASROOT=1` (decision D4 — DSM's socket is root-owned with no
group to hand over, and a `chgrp` is undone by the next DSM update).

That is accepted rather than worked around, because the job's actual purpose —
build images and recreate containers on this host — *is* root-level work: it is
the same work the manual procedure in [`../README.md`](../README.md) does from
`sudo -i`. What protects the NAS is therefore **reach**, not privilege:

- `release.yml` is the **only** workflow that names `runs-on: [self-hosted, nas]`.
  A new workflow that names it is a review finding, not a configuration change.
- The runner mounts **nothing** beyond the socket, the clone and its own state
  directory.
- While the repository is public: the trigger is a **tag push only** (a fork
  cannot push a tag here), `release.yml` checks `github.repository` and
  `github.actor` before it runs anything, Actions is set to "allow selected
  actions", and fork-PR runs require approval.
- After the repository goes private, one gap opens that no setting on a Free plan
  closes: without runner groups, **any** workflow on **any** branch in the
  repository can name `runs-on: self-hosted` and land here. The three rules above
  are the containment. Spec 38's "Going-private checklist" has the rest.

If you ever need this runner to stop being able to do any of that, the honest
answer is `docker-compose -f docker-compose.runner.yml down` — not a narrower
mount.

## Install: the first start

The runner registers itself **once**, with a registration token that GitHub
expires after an hour. Nothing long-lived is stored on the NAS: after the first
start the container authenticates with its own registration, which is scoped to
this one repository.

**1. Mint the token on your own machine** (it needs admin on the repository;
`gh` is already authenticated there):

```console
$ gh api -X POST repos/CDRO/Inventory/actions/runners/registration-token -q .token
ABCDEFGHIJKLMNOPQRSTUVWXYZ1234
```

The same token is what GitHub shows under **Settings → Actions → Runners → New
self-hosted runner**, if you would rather read it off the page.

**2. On the NAS, start the runner once with it**, from this folder:

```console
$ sudo -i
$ cd /volume1/docker/inventory/deploy/synology/runner
$ RUNNER_TOKEN=ABCDEFGHIJKLMNOPQRSTUVWXYZ1234 docker-compose -f docker-compose.runner.yml up -d --build
```

`RUNNER_TOKEN` is passed on the command line and **not** put in `.env`, for the
same reason `TS_AUTHKEY` is not: `.env` is rewritten wholesale from
`.env.example` by every `setup` run, so a one-off value placed there is either
dropped silently or re-prompted forever. It lands in your shell history and, via
interpolation, in `docker inspect` until the container is recreated; it is
single-use and expires within the hour, and the entrypoint drops it from the
environment before the listener — and therefore before any job — exists.

No `-p` is needed and none should be given: the project name `inventory-runner`
is in the file. That is what keeps `dc down` on the app stack from stopping the
runner, and recreating the runner from disturbing the app stack.

**If your clone is not at `/volume1/docker/inventory`**, pass the path too, on
every command that touches this file:

```console
$ INVENTORY_CLONE=/volume2/docker/inventory RUNNER_TOKEN=… docker-compose -f docker-compose.runner.yml up -d --build
```

The clone is mounted **at the identical path inside and out**, and that is
load-bearing rather than tidy: `docker-compose.nas.yml`'s bind mounts
(`./pgdata`, `./uploads`, `./imagecache`, `./backups`) are relative to the clone,
and a container the runner starts through the socket has those paths resolved by
the **host's** daemon, not by the runner's filesystem. A clone mounted anywhere
else would bind the wrong host directories — successfully, and without a word.
Both halves of that line in `docker-compose.runner.yml` interpolate the same
variable; do not edit one half.

## Verify

The start-up smoke test is the first thing in the log, and a runner that fails it
has taken no work and registered nothing:

```console
$ docker-compose -f docker-compose.runner.yml logs runner
smoke: ok    docker-compose 2.31.0 (>= 2.24)
smoke: ok    git version 2.55.0
smoke: ok    docker socket /var/run/docker.sock answers (server 24.0.2)
smoke: ok    clone /volume1/docker/inventory is a git checkout at 8127867
smoke: ok    docker-compose config accepts the app stack's two files
runner: smoke test passed
runner: registering "nas-inventory" at https://github.com/CDRO/Inventory with labels self-hosted,nas,synology
…
√ Connected to GitHub
…
Listening for Jobs
```

`Listening for Jobs` is the line that means it is done. Then check GitHub's own
view of it — from your own machine:

```console
$ gh api repos/CDRO/Inventory/actions/runners \
    -q '.runners[] | {name, status, busy, labels: [.labels[].name]}'
{"busy":false,"labels":["self-hosted","Linux","X64","nas","synology"],"name":"nas-inventory","status":"online"}
```

`status: online`, `busy: false` — idle and waiting. The web page for the same
thing is **Settings → Actions → Runners**.

**A reboot needs nothing from you.** `restart: unless-stopped` brings the
container back, and the registration is in `./runner_state`, not in the image or
the container layer — so it comes back as the same runner, with no token and no
human. That is the whole reason the registration is persistent rather than
`--ephemeral`.

## Everyday operation

| You want to | Command (from this folder, `sudo -i`) |
|---|---|
| see whether it is running | `docker-compose -f docker-compose.runner.yml ps` |
| read the log / watch a job | `docker-compose -f docker-compose.runner.yml logs -f runner` |
| stop it taking new work | `docker-compose -f docker-compose.runner.yml stop` |
| start it again | `docker-compose -f docker-compose.runner.yml up -d` |

A stop is graceful: the listener traps `SIGTERM` and finishes the job in flight
before exiting. It runs **one job at a time**, which a single runner process is by
construction — there is no setting for it and no second instance.

## Upgrade

Two different things update, on two different schedules:

- **The runner agent updates itself**, inside the container, whenever GitHub
  requires a newer version. Nothing to do.
- **The tools beside it** — `docker-compose`, the Docker CLI, git — are pinned in
  the `Dockerfile` and only change when you rebuild:

```console
$ cd /volume1/docker/inventory/deploy/synology/runner
$ docker-compose -f docker-compose.runner.yml build --pull
$ docker-compose -f docker-compose.runner.yml up -d
```

The state directory is untouched by that, so **the registration survives** a
rebuild and a recreate; the container comes back as the same runner without a
token. Run it after a `git pull` that changed anything in this folder.

## Remove

Two steps, and the order matters — de-register **while** the container still has
its credentials. Mint the removal token on your own machine:

```console
$ gh api -X POST repos/CDRO/Inventory/actions/runners/remove-token -q .token
ZYXWVUTSRQPONMLKJIHGFEDCBA9876
```

then, on the NAS:

```console
$ cd /volume1/docker/inventory/deploy/synology/runner
$ docker exec -w /home/runner inventory-runner ./config.sh remove --token ZYXWVUTSRQPONMLKJIHGFEDCBA9876
$ docker-compose -f docker-compose.runner.yml down
$ rm -rf runner_state
```

If the container is already gone and cannot de-register itself, remove the runner
from GitHub's side instead — from your own machine — and then delete the state
directory, which is now worthless:

```console
$ gh api repos/CDRO/Inventory/actions/runners -q '.runners[] | "\(.id)\t\(.name)"'
$ gh api -X DELETE repos/CDRO/Inventory/actions/runners/<id>
```

Leaving `runner_state` behind after either route is what produces the confusing
case: a container that starts, finds a registration GitHub no longer knows, and
fails to connect. Delete the directory and register again with a fresh token.

## When something is wrong

| The log says | What it means |
|---|---|
| `smoke: FAIL … is not a socket inside the container` | The socket mount is missing. You are running a modified `docker-compose.runner.yml`, or with `-f` pointing somewhere else. |
| `smoke: FAIL … the daemon does not answer` | The socket is mounted but Docker on the host is not running. This is a DSM problem, not a runner problem. |
| `smoke: FAIL the clone is not mounted at …` | `INVENTORY_CLONE` does not match the clone's real path. Compose **creates** a missing bind-mount source as an empty directory rather than failing, which is exactly the silent mistake this check exists to catch. |
| `smoke: FAIL … is not a git checkout` | The path is a directory but not a clone. `update --ref` fetches through the clone's own remote, so it needs the real thing. |
| `smoke: FAIL docker-compose cannot parse …` | The app stack's compose files do not parse with the pinned Compose. A deploy would have failed in the middle; fix the files (or the pin) first. |
| `FATAL: this runner is not registered yet and RUNNER_TOKEN is empty` | First start without a token, or `runner_state` was deleted. Mint a token and start once with it. |
| GitHub shows the runner `offline` | The container is stopped, or the NAS is off the network. `logs -f runner` says which. |
| GitHub shows **two** runners, one offline | A start with a different `RUNNER_NAME` registered a second one. Delete the offline one by id (above); the name is meant to stay `nas-inventory`. |

To check the image without registering anything or taking a job — after a
rebuild, or when you suspect the tools rather than the registration — run the
smoke test on its own. It exits `0` and does nothing else:

```console
$ docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v /volume1/docker/inventory:/volume1/docker/inventory \
    -e RUNNER_SMOKE_ONLY=1 \
    inventory-runner:local
```

This is also what CI runs on every change to this folder
(`.github/workflows/test.yml`, job `runner-image`), so the Dockerfile cannot rot
unnoticed between releases.
