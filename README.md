# Inventory

Self-hosted, AI-powered household inventory system.

`docs/specs/` is the implementation contract; start at
[`docs/specs/00-overview.md`](docs/specs/00-overview.md). This file covers only
how to run the thing.

## Requirements

**Docker and Docker Compose v2.24 or newer. Nothing else.** No Go, Node,
Python, or Java is installed on the host — every build, test, lint, and
migration is a Docker invocation
([`docs/specs/01-architecture-and-deployment.md`](docs/specs/01-architecture-and-deployment.md)).

The version floor is real: the compose files use the long-form `env_file` with
`required: false`, and older Compose rejects the whole file rather than
ignoring the key. Synology's Container Manager has shipped older versions —
check with `docker compose version` before deploying.

## First run

```bash
docker compose run --rm setup    # interactive wizard, writes ./.env
docker compose up -d
```

`setup` reads `.env.example` as the canonical variable list, prompts for each
variable with its default, and generates `SESSION_SECRET` itself.
`DATABASE_URL` is not prompted for either — it is derived from the
`POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` answers once every
prompt is answered. Run it before starting the stack: Compose reads `.env` when it parses the file, so a
running container will not pick up a `.env` written afterwards. If you rerun
setup on a live stack, apply the change with:

```bash
docker compose up -d --force-recreate
```

Starting without a `.env` is not fatal to Compose, but the `app` container
exits immediately with the variables it needs and the two commands that fix
it.

### The first account

There is no sign-up. Once migrations have run (`migrate up`, below), the
server creates one admin from `ADMIN_INITIAL_USERNAME` and
`ADMIN_INITIAL_PASSWORD` — only while the users table is empty, so changing
those values later does nothing. Log in with them, then open `/admin` to
create the household's accounts and storages and decide who can see which.
An admin is not automatically a member of any storage; add yourself to one to
use it.

`/admin` is reached by typing the URL — the app never links to it — and to
anyone who is not an admin it is an ordinary 404
([`docs/specs/03-auth-and-multi-tenancy.md`](docs/specs/03-auth-and-multi-tenancy.md)).

## Everyday commands

| Task | Command | Image |
|---|---|---|
| Build | `docker compose build` | dev |
| Run (dev) | `docker compose up -d` | dev |
| Unit tests | `docker compose run --rm app go test ./...` | dev |
| Lint / vet | `docker compose run --rm app go vet ./...` | dev |
| Run (production) | `docker compose -f docker-compose.yml up -d` | prod |
| Migrations | `docker compose -f docker-compose.yml run --rm app migrate up` | prod |
| Migration state | `docker compose -f docker-compose.yml run --rm app migrate status` | prod |
| First-time setup | `docker compose run --rm setup` | prod |
| Backup | `docker compose -f docker-compose.yml run --rm backup` | postgres |
| Restore | `docker compose -f docker-compose.yml run --rm backup restore /backups/<archive>` | postgres |
| Health | `curl localhost:8000/healthz` | — |

**The `-f docker-compose.yml` pin is not decoration.** Compose auto-loads
`docker-compose.override.yml`, so a bare command gets the Go-capable dev image
— which is exactly what `go test` needs and exactly what `migrate` cannot use,
since `migrate` is a subcommand of the compiled binary that only the production
image carries. Commands are grouped by image above so it is clear which is
which.

The frontend has no build, install, or lint command — it is plain HTML, CSS,
and ES modules served as-is. In dev, `STATIC_DIR` points the server at
`web/static` on disk, so editing a file and refreshing the browser is enough.

## Compose files

- `docker-compose.yml` — base and **production**. The `app` service is the
  `scratch` image: ~20MB, no shell, no toolchain.
- `docker-compose.override.yml` — local dev. Compose loads this automatically,
  which is what puts the Go-capable `dev` image under the `app` name so
  `docker compose run --rm app go test ./...` works. Production is unaffected
  because it pins the base file explicitly with `-f docker-compose.yml`.

### Deploying to the NAS

```bash
docker compose -f docker-compose.yml build
docker compose -f docker-compose.yml up -d
```

> **The operator's Synology DS923+ is a different setup** — bind-mounted data and
> a Tailscale sidecar instead of Traefik, as one extra compose file. On it, the
> commands above are the wrong ones (they start Traefik on DSM's port 80); use
> `docker-compose -p inventory -f docker-compose.yml -f docker-compose.nas.yml …`
> instead (`docker-compose` with the hyphen). `deploy/synology/` has scripts for
> this — `install-shell` (gives you `$DC` and `dc`) and `update` (a safe rolling
> update), documented in [`deploy/synology/README.md`](deploy/synology/README.md).
> See "Synology NAS variant" in
> [`docs/specs/01-architecture-and-deployment.md`](docs/specs/01-architecture-and-deployment.md).

Before deploying, run the E2E gate
(`docker-compose.e2e.yml`, [`docs/specs/05-frontend-pwa-foundations.md`](docs/specs/05-frontend-pwa-foundations.md)):
[`docs/specs/01-architecture-and-deployment.md`](docs/specs/01-architecture-and-deployment.md)
calls a deployment that skips it invalid, not merely discouraged.

**The pin is a security control.** Without it the NAS runs the `dev` target,
which sets `APP_ENV=dev` — enabling `debug_reason` disclosure in error
responses ([`docs/specs/03-auth-and-multi-tenancy.md`](docs/specs/03-auth-and-multi-tenancy.md))
— ships a toolchain image instead of the `scratch` binary, and publishes port
8000 past Traefik. On a Tailscale-only host that last one is reachable to the
whole tailnet without the ingress. A deployment procedure that omits the pin is
a broken deployment.

Remote access is Tailscale by default, installed as a Synology package outside
this compose file. A commented-out `cloudflared` service is kept in
`docker-compose.yml` as the documented alternative; exactly one method should
be active at a time.

### Why `.env` is optional to Compose

`docker compose run --rm setup` is the command that *writes* `.env`, and
Compose validates the whole project model before running anything — so a
required `env_file` makes the documented first step fail on a fresh clone. The
optional declaration is also what lets the container reach its own startup
check and print the remediation above, instead of Compose aborting with a parse
error that names no remedy.

## Backup and restore

Everything here runs through `docker compose`, like everything else — there is
no `pg_dump` and no `tar` on the host
([`docs/specs/15-backup-restore-and-export.md`](docs/specs/15-backup-restore-and-export.md)).
The `backup` service is the `postgres:16-alpine` image the database already
uses, driving [`scripts/backup`](scripts/backup).

### Taking a backup

```bash
docker compose -f docker-compose.yml run --rm backup
# backup: wrote /backups/inventory-backup-2026-09-21-1530.tar.gz (4.1M)
```

The archive lands in `./backups` in the clone and holds two things:

- `db.sql` — a logical `pg_dump` of the whole database: inventory, users,
  logs, settings, catalog.
- `uploads/` — every permanent image.

It deliberately holds **nothing from `imagecache`**, which is re-fetchable by
definition and would otherwise be most of the bytes, and **no `.env`**, which
holds `SESSION_SECRET` and the API keys. An archive travels; secrets should not
travel with it. **Keep a copy of `.env` somewhere else** — not because the
restore needs the same values (it does not), but because losing the API keys is
its own bad afternoon.

The command exits non-zero, with the reason on stderr, if the database is
unreachable or the archive cannot be written, and it writes the file under a
temporary name and renames it only on success — so a failed run never leaves
something that looks like a backup. That matters because scheduling is yours:
point DSM's Task Scheduler at exactly the command above. The application does
not schedule its own backups, for the same reason it does not restart its own
containers.

> On the Synology NAS use the two-file prefix instead of `-f docker-compose.yml`
> — `$DC run --rm backup` — as for every other command there. See
> [`deploy/synology/README.md`](deploy/synology/README.md).

### Restoring

Onto a clean checkout with nothing but Docker installed:

```bash
git clone <this repository> inventory && cd inventory
cp /path/to/inventory-backup-YYYY-MM-DD-HHMM.tar.gz ./backups/   # mkdir -p ./backups first

docker compose run --rm setup                                     # writes a new .env
docker compose -f docker-compose.yml up -d db                     # the database, alone
docker compose -f docker-compose.yml run --rm backup restore /backups/inventory-backup-YYYY-MM-DD-HHMM.tar.gz
docker compose -f docker-compose.yml run --rm app migrate up
docker compose -f docker-compose.yml up -d
```

Notes on the steps, in the order you will wonder about them:

- **`setup` answers do not have to match the backed-up instance.** The dump
  carries no database roles or grants, and the restore creates the database
  from whatever `.env` now says. A fresh `SESSION_SECRET` is fine.
- **Everyone logs in again.** The restore clears the session table, so cookies
  and paired devices from before the backup are dead. That is intended: a
  session id is looked up in the database rather than signed, so leaving the
  dump's rows in place would hand back working credentials from before the
  disaster.
- **The restore refuses to run while anything else is connected to the
  database** — that is why only `db` is started in step two. A restore into a
  running stack corrupts both.
- **`migrate up` applies only what is newer than the backup.** The dump carries
  goose's version table, so restoring a current archive is a no-op here, and
  restoring an old one upgrades it.
- **Images work immediately; suggestion thumbnails re-fetch themselves.** The
  `cached_images` rows come back in the dump with no files behind them, and the
  serving path treats a missing file as a cache miss, drops the stale row and
  moves on. There is no cache-specific restore step, and there is never a
  broken image with a dangling row.
- Existing files in the uploads volume are left alone rather than replaced, so
  a restore cannot destroy images belonging to an instance you are still
  salvaging.

### Exporting one storage

Separately from all of the above, any member of a storage can download that
storage as portable files:

```
GET /api/storages/{storage_id}/export
```

It streams a ZIP holding `export.json` (format `inventory-export/1`) and an
`images/` folder — readable without this software, and carrying nothing from
any other storage, no credential material, and no EXIF or GPS metadata. There
is no import endpoint, deliberately: whole-instance moves go through
backup/restore above, and this exists so the data outlives the software.

## Health

`GET /healthz` answers `200` once the database is reachable:

```json
{ "status": "ok", "vision": "ok" }
```

`vision` is reported separately and never changes the status code. A
`model_unavailable` value means the configured `GEMINI_MODEL` is not one the
provider currently offers — vision features return `503`, everything else
keeps working, and an admin can pick a working model without a redeploy.
