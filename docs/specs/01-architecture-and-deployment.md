# 01 — Architecture & Deployment

Depends on: [`00-overview.md`](00-overview.md).

## Tech stack (fixed — do not substitute)

| Layer | Choice |
|---|---|
| Frontend | **Vanilla JavaScript (ES modules), hand-written CSS, plain HTML.** No Node.js, npm, React, Next.js, TypeScript, CSS framework, bundler, transpiler, or build step of any kind. Files are served to the browser exactly as they exist in the repository. |
| Backend | **Go** — `net/http` with the `chi` router, `pgx` for PostgreSQL, `goose` for migrations, `html/template` for the server-rendered admin UI, `embed.FS` for shipping the frontend inside the binary |
| Database | PostgreSQL 16 (with the `pg_trgm` extension, used for product matching) |
| Vision LLM | Google Gemini, Flash tier (see AI model resilience, below) |
| Product image search | SerpAPI, Google Images engine (see `07-shopping-list-reconciliation.md`) |
| Icon/vector suggestion | Iconify API (free, no key required) (see `07-shopping-list-reconciliation.md`) |
| Internal routing | Traefik (single ingress for the whole stack) |
| Containerization | Docker Compose |
| Target host | Synology DSM NAS, Docker Container Manager |
| Remote access | Tailscale |

### Why Go, and what it costs

The backend is a static binary in a `FROM scratch` container: ~20MB image,
low idle memory (relevant on a NAS), no language runtime to keep patched,
and a single artifact that is identical locally and in production. Two
consequences that **must** be handled explicitly, or the app breaks in
non-obvious ways:

- `scratch` contains no CA certificate bundle. The outbound HTTPS calls to
  Gemini, SerpAPI, and Iconify fail with a certificate-verification error
  unless `/etc/ssl/certs/ca-certificates.crt` is copied from the builder
  stage into the final image.
- `scratch` contains no timezone database. Copy `/usr/share/zoneinfo` from
  the builder (or import `time/tzdata` in `main.go` to embed it) — expiry
  date arithmetic in `08-expiration-and-classification.md` depends on it.

## No-host-toolchain constraint

**No language runtime may be assumed on the host** — no Go, no Node.js, no
Python, no Java, for setup, building, testing, migrations, or one-off
scripts. Only `docker` and `docker compose` may be assumed present.

Every command is therefore a Docker invocation:

| Task | Command |
|---|---|
| Interactive first-time setup | `docker compose run --rm setup` |
| Build everything | `docker compose build` |
| Run the stack | `docker compose up -d` |
| Run unit tests | `docker compose run --rm app go test ./...` |
| Run E2E tests (deployment gate) | `docker compose -f docker-compose.e2e.yml run --rm e2e` |
| Run migrations | `docker compose run --rm app /inventory migrate up` |
| Lint / vet | `docker compose run --rm app go vet ./...` |

The frontend has **no** build, install, or lint command, because it has no
toolchain — it is plain files. Browser behavior is covered by end-to-end
tests that run in a throwaway pulled container: **not** part of the image
build or the dev loop, but **required to pass before deploying**. See
`05-frontend-pwa-foundations.md` for the suite and its required coverage.

## Repository layout

```
/
├── docs/
│   ├── specs/                  # this document set — implementation source of truth
│   └── explanations/           # human-facing explanations — NOT for agents, see its README
├── cmd/
│   └── inventory/
│       └── main.go             # entrypoint; subcommands: serve (default), setup, migrate
├── internal/
│   ├── config/                 # env loading + DB-backed settings overrides
│   ├── httpapi/                # chi routers, handlers, middleware (auth, storage scoping)
│   ├── admin/                  # server-rendered admin UI handlers (html/template)
│   ├── store/                  # pgx queries, one file per table group
│   ├── vision/                 # Gemini client, prompts, response parsing, model resilience
│   ├── imagesearch/            # SerpAPI + Iconify clients
│   ├── matching/               # shared product matching (catalog-first, then trigram)
│   ├── jobs/                   # background job runner + job store
│   └── export/                 # CSV/PDF generation
├── web/
│   ├── static/                 # vanilla JS/CSS/HTML — served as-is, embedded via embed.FS
│   └── templates/              # html/template files for the server-rendered admin UI
├── migrations/                 # goose SQL migrations
├── Dockerfile                  # multi-stage: builds and tests everything, outputs scratch image
├── docker-compose.yml          # base/production
├── docker-compose.dev.yml      # dev overrides
├── .env.example
└── PROJECT_PLAN.md             # original informal notes — superseded by docs/specs/
```

## Single self-contained Dockerfile

One `Dockerfile` performs the entire installation and build process, so
that anything which builds locally deploys unchanged to the NAS. Nothing
is installed on the host.

```dockerfile
# ---- builder ----
FROM golang:1-alpine AS builder
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/inventory ./cmd/inventory

# ---- dev stage (used by docker-compose.dev.yml) ----
FROM golang:1-alpine AS dev
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
CMD ["go", "run", "./cmd/inventory", "serve"]

# ---- production ----
FROM scratch AS prod
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /out/inventory /inventory
EXPOSE 8000
ENTRYPOINT ["/inventory"]
CMD ["serve"]
```

The frontend needs no build stage: `web/static` is embedded into the
binary by `embed.FS` at compile time. In dev, setting `STATIC_DIR=/src/web/static`
makes the server read those files from disk instead, so editing a `.js` or
`.css` file and refreshing the browser is enough — no rebuild, no watcher.

## Interactive setup (`docker compose run --rm setup`)

`.env` is created by an interactive wizard, not hand-copied, so no
variable is silently missed:

- Implemented as the `setup` subcommand of the same Go binary, so it works
  identically on Windows, macOS, and Linux — the prompts run inside the
  container, never in the host shell (which is why no host runtime and no
  shell-script portability problem exists).
- Reads `.env.example` as the canonical variable list, prompts for each
  variable showing its default, and accepts the default on empty input.
- Auto-generates `SESSION_SECRET` (cryptographically random) rather than
  prompting for it.
- Refuses to overwrite an existing `.env` unless explicitly confirmed, and
  writes it to the bind-mounted project directory.
- Validates obviously-wrong input (empty API keys, malformed
  `DATABASE_URL`) and re-prompts.

The `setup` service in `docker-compose.yml` mounts the project directory
and runs with `stdin_open: true` / `tty: true` so prompts work.

### First-run order, and what happens if it is skipped

Setup must run **before** the stack starts, because Docker Compose reads
`.env` at file-parse time — a running container cannot pick up a `.env`
written afterwards without being recreated. The system therefore fails
fast and says so, rather than starting half-configured:

```console
$ docker compose run --rm setup      # writes ./.env interactively
$ docker compose up -d
```

**If `docker compose up` is run first (no `.env` yet):** compose aborts on
the missing `env_file`, which is correct but cryptic. To make the message
actionable, the `app` container additionally validates its configuration
at startup and, on missing/empty required variables, exits non-zero with:

```
No configuration found (DATABASE_URL, SESSION_SECRET, GEMINI_API_KEY are unset).
Run:  docker compose run --rm setup
Then: docker compose up -d
```

`restart: unless-stopped` must not turn this into a crash loop: the
config error is a **fatal, non-retryable** exit, so the container exits
with a distinct code and the message stays readable in
`docker compose logs app`.

**If setup is run while the stack is already up**, the new `.env` is not
picked up by running containers. The `setup` command detects this case as
"an `.env` already existed / the stack may be running" and ends by
printing the exact command to apply the change:

```
.env written. Apply it with:
  docker compose up -d --force-recreate
```

The application deliberately does **not** restart containers itself. Doing
so would require mounting `/var/run/docker.sock` into the app container,
which hands full host-level Docker control to the web application — an
unacceptable trade for saving one command. Configuration that genuinely
needs to change at runtime (the Gemini model) is handled instead by the
`settings` table, which requires no restart at all; see AI model
resilience below.

## `.env.example`

```dotenv
# --- Runtime ---
APP_ENV=prod                  # "dev" enables verbose error reasons (see 03/04)
HTTP_PORT=8000
STATIC_DIR=                   # empty = serve embedded assets; dev sets a path

# --- Database ---
POSTGRES_USER=inventory
POSTGRES_PASSWORD=changeme
POSTGRES_DB=inventory
DATABASE_URL=postgres://inventory:changeme@db:5432/inventory?sslmode=disable

# --- Auth ---
SESSION_SECRET=                # generated by `docker compose run --rm setup`
ADMIN_INITIAL_USERNAME=admin
ADMIN_INITIAL_PASSWORD=changeme-set-on-first-boot

# --- Vision LLM ---
GEMINI_API_KEY=
GEMINI_MODEL=gemini-2.0-flash  # may be overridden in-app; see "AI model resilience"

# --- Shopping-list image suggestions ---
SERPAPI_API_KEY=
```

There is no frontend API-URL variable: Traefik serves the UI and the API
from the same origin, so the frontend calls relative paths (`/api/...`).

Secrets live only in `.env` (git-ignored) on the Docker host. No secret may
be hardcoded in a Dockerfile, compose file, template, or source file.

## AI model resilience

Pinned model ids get deprecated and then simply stop working. The app must
degrade visibly rather than failing opaquely:

- **Effective model** = the value in the `settings` table (key
  `gemini_model`) if present, otherwise `GEMINI_MODEL` from the
  environment. The DB value always wins, so a model can be corrected
  in-app without redeploying.
- **On startup and on every model-not-found error**, the backend calls the
  Gemini `models.list` endpoint (`GET https://generativelanguage.googleapis.com/v1beta/models`)
  and caches the list of available model ids.
- **If the effective model is not in that list**, the app still starts and
  all non-vision features keep working. Vision endpoints
  (`06`, `07`, `09`) return the error code `model_unavailable` with a
  message naming the configured model, and the admin UI shows a persistent
  banner listing the currently available models.
- **Admin remediation**, from the admin UI (`03-auth-and-multi-tenancy.md`):
  1. Select a different model from the fetched list and save — written to
     `settings`, applied immediately, no restart; or
  2. Download a regenerated `.env` file with the corrected `GEMINI_MODEL`
     line, for operators who prefer redeploying from config.
- `GET /healthz` reports `{"status":"ok","vision":"ok"|"model_unavailable"}`
  so the degraded state is visible without opening the UI.

## `docker-compose.yml` (base/production)

```yaml
services:
  traefik:
    image: traefik:v3
    command:
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
      - --entrypoints.web.address=:80
    ports:
      - "80:80"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    restart: unless-stopped

  app:
    build:
      context: .
      target: prod
    env_file: .env
    depends_on:
      db:
        condition: service_healthy
    labels:
      - traefik.enable=true
      - traefik.http.routers.app.rule=PathPrefix(`/`)
      - traefik.http.services.app.loadbalancer.server.port=8000
    volumes:
      - uploads:/data/uploads
    restart: unless-stopped

  setup:
    build:
      context: .
      target: prod
    entrypoint: ["/inventory", "setup"]
    volumes:
      - .:/work
    working_dir: /work
    stdin_open: true
    tty: true
    profiles: ["tools"]      # only runs when invoked explicitly

  db:
    image: postgres:16-alpine
    env_file: .env
    environment:
      POSTGRES_USER: ${POSTGRES_USER}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      POSTGRES_DB: ${POSTGRES_DB}
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U $${POSTGRES_USER} -d $${POSTGRES_DB}"]
      interval: 5s
      timeout: 5s
      retries: 10
    restart: unless-stopped

volumes:
  pgdata:
  uploads:
```

The single `app` service serves everything: the static frontend at `/`,
the JSON API at `/api`, and the server-rendered admin UI at `/admin`.
Traefik is the only ingress, which is what makes the remote-access layer
swappable (below).

## `docker-compose.dev.yml` (local staging overrides)

Used as `docker compose -f docker-compose.yml -f docker-compose.dev.yml up`
on the operator's PC with Docker Desktop.

```yaml
services:
  app:
    build:
      context: .
      target: dev
    environment:
      - APP_ENV=dev
      - STATIC_DIR=/src/web/static
    volumes:
      - ./web:/src/web          # edit frontend files, just refresh the browser
      - ./internal:/src/internal
      - ./cmd:/src/cmd
    ports:
      - "8000:8000"             # direct access, bypassing Traefik, for debugging
```

## Deployment model

- **Staging:** operator's PC, Docker Desktop, dev compose overrides,
  `APP_ENV=dev`, frontend served from disk.
- **Production:** Synology NAS, Container Manager, base `docker-compose.yml`
  only. Because the Dockerfile performs the whole build, deploying is
  `docker compose build && docker compose up -d` on the NAS — or building
  elsewhere and pulling from a registry; both are supported and require no
  additional tooling.
- **Deployment gate:** the end-to-end browser suite
  (`05-frontend-pwa-foundations.md`) must pass before an image is
  promoted to production. Unit tests already run inside the image build;
  E2E runs separately because it needs a live stack. A deployment that
  skips it is not a valid deployment.

## Remote access (interchangeable by configuration)

Traefik is the sole ingress and terminates all internal routing. The
remote-access layer attaches to Traefik's `web` entrypoint and is therefore
a **drop-in, swappable component: changing it is a compose/config edit with
zero application changes.** Whichever is active, application-level session
auth (`03-auth-and-multi-tenancy.md`) still applies — network access
control is never a substitute for it.

- **Active default — Tailscale:** installed as a Synology package, joining
  the NAS to a private WireGuard network. No router ports are opened; the
  app is reached at the NAS's Tailscale address. Nothing in the
  application is Tailscale-specific.
- **Documented alternative — Cloudflare Tunnel:** if Tailscale proves
  impractical, add a `cloudflared` service to the compose file pointing at
  `http://traefik:80` and disable/ignore the Tailscale package. Keep this
  service present but commented out in `docker-compose.yml`, with a note
  that exactly one remote-access method should be active at a time.

Because the app must work on a LAN-only / Tailscale-only NAS with no
inbound internet exposure, the frontend must not depend on any CDN at
runtime — see the vendoring rule in `11-reporting-and-analytics.md`.

## Health/readiness

`app` exposes `GET /healthz` returning `200` once it can reach the
database, with the vision-subsystem status described under AI model
resilience. Used by the compose healthcheck pattern and by Traefik.
