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
variable with its default, and generates `SESSION_SECRET` itself. Run it
before starting the stack: Compose reads `.env` when it parses the file, so a
running container will not pick up a `.env` written afterwards. If you rerun
setup on a live stack, apply the change with:

```bash
docker compose up -d --force-recreate
```

Starting without a `.env` is not fatal to Compose, but the `app` container
exits immediately with the variables it needs and the two commands that fix
it.

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

## Health

`GET /healthz` answers `200` once the database is reachable:

```json
{ "status": "ok", "vision": "ok" }
```

`vision` is reported separately and never changes the status code. A
`model_unavailable` value means the configured `GEMINI_MODEL` is not one the
provider currently offers — vision features return `503`, everything else
keeps working, and an admin can pick a working model without a redeploy.
