# Inventory

Self-hosted, AI-powered household inventory system.

`docs/specs/` is the implementation contract; start at
[`docs/specs/00-overview.md`](docs/specs/00-overview.md). This file covers only
how to run the thing.

## Requirements

**Docker and Docker Compose. Nothing else.** No Go, Node, Python, or Java is
installed on the host — every build, test, lint, and migration is a Docker
invocation ([`docs/specs/01-architecture-and-deployment.md`](docs/specs/01-architecture-and-deployment.md)).

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

| Task | Command |
|---|---|
| Build | `docker compose build` |
| Run (dev) | `docker compose up -d` |
| Run (production) | `docker compose -f docker-compose.yml up -d` |
| Unit tests | `docker compose run --rm app go test ./...` |
| Lint / vet | `docker compose run --rm app go vet ./...` |
| Migrations | `docker compose run --rm app migrate up` |
| Migration state | `docker compose run --rm app migrate status` |
| Health | `curl localhost:8000/healthz` |

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

Three details differ from the command table in spec 01, because that table and
the spec's own Dockerfile disagree:

- **`migrate`** is invoked as `docker compose run --rm app migrate up`, not
  `... app /inventory migrate up`. The image sets `ENTRYPOINT ["/inventory"]`,
  so naming the binary again passes it as the subcommand.
- **The dev overrides live in `docker-compose.override.yml`**, not
  `docker-compose.dev.yml`, so the documented test command resolves to an
  image that actually contains Go.
- **`.env` is optional at the Compose layer.** It has to be: the command that
  writes it is itself a Compose service, and a required `env_file` aborts the
  project model before `setup` can run.

## Health

`GET /healthz` answers `200` once the database is reachable:

```json
{ "status": "ok", "vision": "ok" }
```

`vision` is reported separately and never changes the status code. A
`model_unavailable` value means the configured `GEMINI_MODEL` is not one the
provider currently offers — vision features return `503`, everything else
keeps working, and an admin can pick a working model without a redeploy.
