# The Claude Code agent runtime

This folder is a container that runs `claude` — and, later, H19's headless
loop over the issue queue — against this repository, on the same Docker
daemon as the host. It refuses to start until
[`scripts/doctor`](../../scripts/doctor) passes, which is the whole reason it
exists: "I started running claude in a docker container that could not run
other docker containers because of rights" — a failure that used to surface
minutes into a session now surfaces as the container not starting at all.

Design and rules: issue #317 ("H18") and
[`docs/plans/2026-09-harness-optimization.md`](../../docs/plans/2026-09-harness-optimization.md),
section 3.4 and decision D10 — fetch it with
`git show origin/harness/optimization-plan:docs/plans/2026-09-harness-optimization.md`
until PR #319 merges it; no spec applies to this package. This file is the
how-to.

| File | What it is |
|---|---|
| `Dockerfile` | Debian slim, `linux/amd64`. Docker CLI + Compose plugin (client only), `gh`, `gosu`, and `claude` via the pinned native installer. |
| `entrypoint.sh` | Joins the mounted socket's group, sets up git identity from the environment, runs `scripts/doctor --json` and refuses on any `FAIL`, then execs what was asked for as the non-root `agent` user. |
| `docker-compose.agent.yml` | Its own Compose project, `inventory-agent`. Never merged with the app stack's files. |
| `.env.agent.example` | Copy to `.env.agent` (gitignored) and fill in. |

## What this container can do

It mounts `/var/run/docker.sock`, exactly like the NAS runner
(`deploy/synology/runner/`) — a process that can talk to that socket can
start a privileged container and reach anything the host's Docker daemon
can. Unlike the runner, it does **not** run as root: `entrypoint.sh` starts
as root only long enough to join the socket's group (its GID is a property
of the host, discovered at container start, never baked into the image),
then execs everything else — `scripts/doctor`, `claude`, whatever command
you give it — as `agent` (uid 1000). The residual reach is still real: from
inside a session, `docker run` can start a container with `-v /:/host` and
read or write anything on the machine. Nothing here pretends otherwise; the
protection is the same as the runner's — **reach**, not privilege. This
container mounts nothing beyond the socket, the repository, and its own
`~/.claude` state volume.

## Install and first run

**1. Copy the env file and fill it in** (see the comments in each file for
where each value comes from):

```console
$ cp deploy/agent/.env.agent.example deploy/agent/.env.agent
$ $EDITOR deploy/agent/.env.agent
```

**2. Export `REPO_PATH`** to this repository's absolute path, from the
repository root. This is **not** read from `.env.agent` — Compose resolves
`${REPO_PATH}` for the bind mount below before `env_file:` is ever applied
inside the container, so it has to be a real shell variable at the moment
you run `docker compose`:

```console
$ export REPO_PATH=$(pwd)
```

**3. Build and run:**

```console
$ docker compose -f deploy/agent/docker-compose.agent.yml run --rm agent
```

With no command, the default is an interactive `claude` session. The first
thing you will see is the doctor's own output — one line per check — before
anything else starts.

## The identical-path mount

`${REPO_PATH}:${REPO_PATH}` — not `.`, and not some fixed in-container path
like `/repo`. This is load-bearing, not tidy, for the same reason as the NAS
runner's own clone mount
(`deploy/synology/runner/docker-compose.runner.yml`): a `docker compose run`
this container starts through the socket has **its own** bind-mount sources
resolved by the **host's** daemon, not by this container's filesystem. Two
consequences:

- A worktree `claude` creates inside the container (`git worktree add
  ../some-issue`, say) lands under `$REPO_PATH` and is therefore visible on
  the host at the exact same path, immediately — no syncing.
- Nested `docker compose` commands the session runs against this
  repository's own stack (`docker compose run --rm app go test ./...`) see
  the same `./pgdata`, `./uploads`, `./imagecache` the host would.

## Verify

**Doctor output with the socket mounted** (from the repository root, after
steps 1–2 above):

```console
$ docker compose -f deploy/agent/docker-compose.agent.yml run --rm agent scripts/doctor
agent-entrypoint: joined agent to group root (gid 0) for /var/run/docker.sock
ok   docker daemon
ok   compose version
ok   container start
ok   build cache volume
ok   ports (HTTP_PORT=8000, TRAEFIK_PORT=80)
ok   gh auth
ok   git identity + line endings
ok   claude CLI
warn disk space                               docker data root (/var/lib/docker) is not on a filesystem this host can measure (e.g. a Docker Desktop VM) - check its disk allocation separately
ok   bind-mount path
agent-entrypoint: doctor passed
agent-entrypoint: starting: scripts/doctor
 1. docker daemon                            ok
 ...
10. bind-mount path                          ok
```

(The `disk space` `warn` above is scripts/doctor's own, documented behaviour
on Docker Desktop, whose data root lives inside a VM the host cannot `df` —
not a sign of anything wrong here.)

**Doctor output with the socket removed** — the failure this whole package
exists to turn into a refusal instead of a mid-session surprise:

```console
$ docker run --rm -e REPO_PATH=$REPO_PATH --env-file deploy/agent/.env.agent \
    -v "$REPO_PATH:$REPO_PATH" -w "$REPO_PATH" inventory-agent:local scripts/doctor
agent-entrypoint: /var/run/docker.sock is not mounted - scripts/doctor below will say so
FAIL docker daemon                            docker info failed - start Docker Desktop or the docker daemon
ok   compose version
warn container start                          skipped: the docker daemon is not reachable (see check 1)
FAIL build cache volume                       docker volume create inventory-go-build-cache (or re-run scripts/doctor --fix)
warn ports (HTTP_PORT=8000, TRAEFIK_PORT=80)  skipped: the docker daemon is not reachable (see check 1)
ok   gh auth
ok   git identity + line endings
ok   claude CLI
ok   disk space
warn bind-mount path                          in a container, but the docker daemon is not reachable to check the bind mount
agent-entrypoint: FATAL: scripts/doctor failed - fix the FAIL line(s) above, then re-run
```

Exit code `1`. Nothing after the doctor ever ran.

**Prove the socket and the identical path together** — from inside a
session, the mounted repository's own stack starts through the forwarded
socket exactly as it would from the host:

```console
$ docker compose -f deploy/agent/docker-compose.agent.yml run --rm agent
$ docker compose run --rm app go test ./internal/config/
ok  	github.com/CDRO/Inventory/internal/config	0.013s
```

## Worktree checkouts

Point `REPO_PATH` at a **primary checkout** (a plain `git clone`), not a
`git worktree add` checkout of one. A worktree's `.git` is a file pointing at
`.../.git/worktrees/<name>` inside the *primary* checkout's `.git`; unless
that path is also mounted at the identical host path, every git command that
needs repository discovery (including scripts/doctor's own identity check)
fails there with `fatal: not a git repository: ...`. `entrypoint.sh`'s own
git-identity setup treats that as non-fatal — see its comment — but the
doctor check still correctly reports `FAIL`, and the container still refuses
to start. This is the one case decision D10's "same path" design does not
cover; H19's own worktrees stay fine because they are created **inside** the
mounted primary checkout, never as a second mount.

## When something is wrong

| The log says | What it means |
|---|---|
| `agent-entrypoint: FATAL: REPO_PATH must be set …` | You skipped step 2 above. `export REPO_PATH=$(pwd)` from the repository root, every session. |
| `FAIL docker daemon … docker info failed` (socket present) | The socket is mounted but the host's Docker daemon is not answering, or `agent` still could not join its group — check `docker-compose.agent.yml`'s socket volume line. |
| `FAIL docker daemon … docker info failed - start Docker Desktop …` (no `agent-entrypoint: joined …` line above it) | The socket is not mounted at all. Expected if you ran a bare `docker run` without the volume — see "Verify" above. |
| `FAIL gh auth` / `FAIL claude CLI` / `FAIL git identity …` | `.env.agent` is missing a value, or wasn't loaded — check `docker compose config` shows it, and that `GH_TOKEN`/`CLAUDE_CODE_OAUTH_TOKEN`/`GIT_AUTHOR_NAME`+`GIT_AUTHOR_EMAIL` are all non-empty. |
| A nested `docker compose run --rm app …` fails with `open .../.env: permission denied` | `.env` was created by a **root**-context container (`docker compose run --rm setup` builds on the `prod` target, which has no non-root `USER`), so it is unreadable by the non-root `agent` user reading it back through the bind mount. One-time fix, from the host: `docker compose run --rm --user root app chmod 644 .env`. |
| `fatal: not a git repository: …` from any git command | `REPO_PATH` points at a `git worktree add` checkout, not the primary clone — see "Worktree checkouts" above. |
| `claude --version` or `claude auth status` fail inside a running session | The image's pinned version (`CLAUDE_VERSION` build arg in `Dockerfile`) is stale, or `CLAUDE_CODE_OAUTH_TOKEN` expired — mint a new one with `claude setup-token` on the host and update `.env.agent`. |

## Upgrade

`claude`, `gh`, `gosu`, and the Docker CLI/Compose plugin are all pinned by
exact version (and, for `gh`/`gosu`, checksum) as `ARG`s at the top of the
tool-installing `RUN` lines in `Dockerfile`. Bump the pin, then:

```console
$ docker compose -f deploy/agent/docker-compose.agent.yml build --pull
```

`claude`'s own auto-updater is disabled inside the image
(`DISABLE_AUTOUPDATER=1`) for the same reason: this image's version is
whatever the `Dockerfile` says, not whatever `claude.ai` serves an hour after
the build.

## The NAS, later

This image is `linux/amd64` and builds and runs unchanged on the DS923+, the
same as the app image. Nothing here does that today — it is `PC first, NAS
later` per decision D10 — but when it does, the difference is only how it is
started (DSM Task Scheduler instead of an interactive terminal), not the
image or `entrypoint.sh`.
