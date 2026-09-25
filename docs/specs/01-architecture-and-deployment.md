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
| Run the stack (dev) | `docker compose up -d` |
| Run the stack (production) | `docker compose -f docker-compose.yml up -d` |
| Run unit tests | `docker compose run --rm app go test ./...` |
| Run E2E tests (deployment gate) | `docker compose -f docker-compose.e2e.yml run --rm e2e` |
| Run migrations | `docker compose -f docker-compose.yml run --rm app migrate up` |
| Lint / vet | `docker compose run --rm app go vet ./...` |
| Rebuild gamification progress from scratch | `docker compose -f docker-compose.yml run --rm app recompute-progress` |

**Which compose context a command runs in matters, and the table above is
explicit about it for a reason.** `docker-compose.override.yml` holds the dev
overrides and Compose loads it automatically, so a bare `docker compose`
invocation puts the Go-capable `dev` image under the `app` name — which is what
makes `go test` and `go vet` work at all, since the production image is
`FROM scratch` and contains no toolchain.

The two commands that need the *production* image therefore pin the base file
with `-f docker-compose.yml` (on the operator's own Synology NAS the pin is the
two-file `-f docker-compose.yml -f docker-compose.nas.yml` instead — see
"Synology NAS variant", and do not use the single-file form there):

- **Migrations**, because `migrate` is a subcommand of the compiled
  `/inventory` binary, which exists only in the production image. Note also
  that the image sets `ENTRYPOINT ["/inventory"]`, so the subcommand is passed
  on its own — naming the binary again would run `/inventory /inventory migrate`.
- **Production deployment**, so the NAS never picks up the dev overrides. This
  pin is load-bearing: without it a bare `up -d` on the NAS would start the
  `dev` target with `APP_ENV=dev`, which enables `debug_reason` disclosure
  (`03-auth-and-multi-tenancy.md`), replace the `scratch` binary with a
  toolchain image, and publish port 8000 past Traefik.

**Minimum Docker Compose version: v2.24.** The compose files use the long-form
`env_file` with `required: false` (below); older Compose rejects the whole file
rather than ignoring the key.

The frontend has **no** build, install, or lint command, because it has no
toolchain — it is plain files. Browser behavior is covered by end-to-end
tests that run in a throwaway pulled container: **not** part of the image
build or the dev loop, but **required to pass before deploying**. See
`05-frontend-pwa-foundations.md` for the suite and its required coverage.

## Continuous integration

A GitHub Actions workflow (`.github/workflows/test.yml`) runs on every push
to `main` and on every pull request. It runs exactly the commands documented
above — `docker compose run --rm app go vet ./...` then
`docker compose run --rm app go test ./...` — against an ephemeral `.env`
generated at the start of the job (throwaway credentials, never committed,
never reused outside that run). This does not relax the no-host-toolchain
rule: the runner has no Go, Node, or Postgres installed directly, only
Docker; every command still goes through `docker compose`.

This exists because not every environment that needs a real pass/fail signal
for this suite can start a Docker daemon locally — the `test` job on a
GitHub-hosted runner is the fallback source of truth in that case, since it
runs the identical command against a real daemon.

The workflow also accepts `workflow_dispatch`, so the fallback is not limited
to the post-PR review gate. A session with no local Docker can still develop:
push the work-in-progress branch, then ask for a signal on it directly. Two
things `gh run watch` needs help with outside an interactive terminal — every
agent session: it requires an explicit run id, and `gh run list` can briefly
still show only an older run from the same branch right after dispatch, so
poll by the exact commit SHA under test rather than trusting "the newest run
in the list":

```console
$ git push -u origin <branch>
$ gh workflow run test.yml --ref <branch>
$ SHA=$(git rev-parse HEAD)
$ RUN_ID=""
$ for i in $(seq 1 10); do
    RUN_ID=$(gh run list --workflow=test.yml --branch <branch> --event workflow_dispatch \
      --limit 5 --json databaseId,headSha -q ".[] | select(.headSha == \"$SHA\") | .databaseId" | head -1)
    [ -n "$RUN_ID" ] && break
    sleep 3
  done
$ gh run watch "$RUN_ID" --exit-status
```

This is slower than a local `docker compose run --rm app go test ./...` —
each round-trip costs a push and a runner boot — so it is a fallback, not a
replacement: use local Docker when it is available, and this loop only when
it is not.

**`test.yml` is the merge gate; `e2e.yml` deliberately is not.** A second
workflow, `.github/workflows/e2e.yml`, runs the same suite's E2E counterpart
(`docker-compose.e2e.yml`, `05-frontend-pwa-foundations.md`) on
GitHub-hosted runners for the same Docker-availability reason — but only on
push to `main` and on `workflow_dispatch`, **not** on `pull_request`. A PR's
only required status check is `test.yml` (`go vet` + `go test`): E2E is the
pre-deployment gate described in "Deployment model" below, not a per-PR one
— it is materially slower (a full stack plus a real browser) and running it
on every push would make ordinary review cycles wait on it for no benefit,
since `review-tests` (the ship loop's test reviewer) already treats E2E
journeys as work it explicitly could not verify locally rather than a
blocking requirement. Anyone who wants an E2E signal on a specific branch
before it merges triggers `workflow_dispatch` on it explicitly, using the
same dispatch-and-poll recipe as `test.yml`, above.

## Running more than one instance of the stack locally

Two ordinary situations need this: developing two branches side by side, and
the wave orchestrator (`scripts/wellen-orchestrator.ps1`) running several
package worktrees at once. Neither is solved by editing a compose file —
it is solved by giving each checkout's `.env` its own values for three
variables that `docker compose` and the app both already read:

- **`COMPOSE_PROJECT_NAME`** — a Compose-native variable (not one the app
  reads). Setting it in a checkout's `.env` namespaces that checkout's
  containers, network, and named volumes away from every other checkout's,
  with no compose-file change needed at all. Left unset, Compose derives it
  from the directory name, which already differs between worktrees — setting
  it explicitly just makes that guarantee visible and independent of the
  directory naming happening to stay unique. A directory name is free-form
  and Compose's project-name rule is not (lowercase alphanumeric, hyphens and
  underscores only, must start with a letter or number), so a value built
  from one — as the wave orchestrator's does, `<repo>-<slug>` — has to be
  sanitized before it is written, or Compose refuses it outright and every
  `docker compose` command in that worktree fails before it does anything
  (`scripts/wellen-orchestrator.ps1`'s `Get-SanitizedProjectName`).
- **`HTTP_PORT`** — already an application variable
  (`internal/config/config.go`): the app listens on whatever this says, not
  a hardcoded `8000`. `docker-compose.override.yml`'s port mapping reads the
  same variable on both sides (`${HTTP_PORT:-8000}:${HTTP_PORT:-8000}`), so
  a checkout with a distinct `HTTP_PORT` publishes a distinct host port
  automatically — no separate "which port did I map this to" bookkeeping.
- **`TRAEFIK_PORT`** — new, Compose-native, defaults to `80`. Only the host
  side of `docker-compose.yml`'s `traefik` port mapping reads it
  (`${TRAEFIK_PORT:-80}:80`); Traefik's own listener stays on `80`
  internally. A real deployment never sets this, so production keeps
  publishing `80` unchanged.

`db` needs no such variable: it publishes no host port at all, only the
internal Docker network Compose already namespaces per `COMPOSE_PROJECT_NAME`.

None of this needs a compose-file flag on every invocation — Compose reads
`.env` from the working directory automatically, for both its own special
variables and the applications's — so setting these three lines once in a
checkout's `.env` is enough; every `docker compose` command run from that
checkout picks them up for free. The wave orchestrator sets exactly these
three per package worktree, deterministically, so two package sessions
running `docker compose up -d` at the same time — or a crashed session's
containers left running — never collide on a host port or a container name.

**Removing what a worktree leaves behind.** Every such checkout leaves, per
Compose project, a database container, a network, three named volumes
(`pgdata`, `uploads`, `imagecache`) and the images it built. Two things remove
this, at different times, for the wave orchestrator's own worktrees.

The moment a package's issue closes, the orchestrator itself runs
`docker compose down -v --remove-orphans` inside that worktree
(`Stop-PackageStack`) — no waiting for the wave to finish, so a long or
parallel wave never accumulates containers, networks or bound host ports from
packages that are already done. This only ever reaches the worktree's own
default Compose project, never a further project a session started under a
name of its own for some check.

What that step cannot reach — built images, such an extra project, and
anything left by a package whose session crashed before its issue ever
closed — `scripts/wellen-docker-cleanup.ps1` removes instead, for the
worktrees of a finished wave (the orchestrator runs it after the wave's
consolidation for every wave with `"dockerCleanup": true`; by hand:
`.\scripts\wellen-docker-cleanup.ps1 -Wave <n> -DryRun`, then without `-DryRun`).
A real run refuses unless the wave issue is closed, and stops before removing
anything if Docker cannot be listed. It decides ownership from Docker's own
labels, not from names: a Compose project belongs to the wave if one of its
containers has a `com.docker.compose.project.working_dir` inside one of the
wave's worktrees (which also catches a project a session started under a name of
its own), or if its name is the orchestrator's `<repo>-<slug>` or that followed by
a hyphen and more (which catches the network, volumes and images of a project
whose containers are already gone). A slug never claims a longer sibling
(`w5-barcode` does not claim `<repo>-w5-barcode-hot-cache`), and a project with a
container outside the wave's worktrees is not claimed by name. It removes those
projects' containers, networks, volumes and image tags, and it **never** touches:

- the main checkout's own stack (any project with a container in the repository
  root, or named after the repository);
- images without such a project label and tag — above all `inventory-app-dev`,
  which the dev override gives one fixed name that every worktree *and* the main
  checkout share — plus `postgres`, `traefik`, `tailscale`, the Playwright image
  and the build cache;
- anything of another repository. A resource that merely mentions a slug but
  cannot be attributed to a worktree is listed as "kept", not removed.

Images are removed by tag and never by id: two projects that built the same
content share an image id, and removing by id would take the other project's tag
with it.

## Repository layout

```
/
├── docs/
│   ├── specs/                  # this document set — implementation source of truth
│   └── explanations/           # human-facing explanations — NOT for agents, see its README
├── cmd/
│   └── inventory/
│       └── main.go             # entrypoint; subcommands: serve (default), setup, migrate, recompute-progress
├── internal/
│   ├── config/                 # env loading + DB-backed settings overrides
│   ├── httpapi/                # chi routers, handlers, middleware (auth, storage scoping)
│   ├── admin/                  # server-rendered admin UI handlers (html/template)
│   ├── store/                  # pgx queries, one file per table group
│   ├── vision/                 # Gemini client, prompts, response parsing, model resilience
│   ├── imagesearch/            # SerpAPI + Iconify clients
│   ├── matching/               # shared product matching (catalog-first, then trigram)
│   ├── expiry/                 # shelf-life resolution chain (08-expiration-and-classification.md)
│   ├── jobs/                   # background job runner + job store
│   └── export/                 # CSV/PDF generation
├── web/
│   ├── static/                 # vanilla JS/CSS/HTML — served as-is, embedded via embed.FS
│   └── templates/              # html/template files for the server-rendered admin UI
├── migrations/                 # goose SQL migrations
├── Dockerfile                  # multi-stage: builds and tests everything, outputs scratch image
├── docker-compose.yml          # base/production
├── docker-compose.override.yml # dev overrides; auto-loaded by Compose
├── docker-compose.nas.yml      # Synology NAS layer, on top of the base file
├── deploy/
│   └── synology/               # NAS scripts (shell setup, update, optional compose shorthand) + Tailscale serve config
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

# ---- dev stage (used by docker-compose.override.yml) ----
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
COPY --from=builder /src/migrations /migrations
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
  prompting for it. `DATABASE_URL` is likewise never prompted for: once every
  `POSTGRES_*` prompt has its final answer, it is derived from
  `POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` with a `net/url`
  builder — host `db`, port `5432`, `sslmode=disable` fixed
  ([`30-setup-wizard-derived-config.md`](30-setup-wizard-derived-config.md)).
- Refuses to overwrite an existing `.env` unless explicitly confirmed, and
  writes it to the bind-mounted project directory.
- Validates obviously-wrong input (an empty required value, a `$` in
  `POSTGRES_PASSWORD` that Compose's `.env` interpolation would read
  differently than `DATABASE_URL` does) and re-prompts.
- The generated `.env` carries no inline comments: a variable's note is
  written on its own line above the variable, because Compose reads a `#`
  after an *empty* value as part of the value
  ([`30-setup-wizard-derived-config.md`](30-setup-wizard-derived-config.md)).

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

**If `docker compose up` is run first (no `.env` yet):** the `app` container
starts and validates its configuration, then exits non-zero with an actionable
message naming the variables and the two commands that fix it:

```
No configuration found (DATABASE_URL, SESSION_SECRET, GEMINI_API_KEY are unset).
Run:  docker compose run --rm setup
Then: docker compose up -d
```

`restart: unless-stopped` must not turn this into a crash loop: the
config error is a **fatal, non-retryable** exit, so the container exits
with a distinct code and the message stays readable in
`docker compose logs app`.

**`.env` is declared optional at the Compose layer** — the long-form
`env_file: [{path: .env, required: false}]` on `app` and `db`. It has to be:
`docker compose run --rm setup` is the command that *writes* `.env`, and
Compose validates the whole project model before running anything, so a
required `env_file` makes the documented first step fail on a fresh clone. The
optional declaration is also what lets the container reach its own startup
check and print the message above, instead of Compose aborting with a parse
error that names no remedy.

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
# "dev" enables verbose error reasons (see 03/04)
APP_ENV=prod
HTTP_PORT=8000
# empty = serve embedded assets; dev sets a path
STATIC_DIR=

# --- Database ---
POSTGRES_USER=inventory
POSTGRES_PASSWORD=changeme
POSTGRES_DB=inventory
# derived from POSTGRES_USER, POSTGRES_PASSWORD and POSTGRES_DB above; never
# prompted for (see 30-setup-wizard-derived-config.md)
DATABASE_URL=postgres://inventory:changeme@db:5432/inventory?sslmode=disable

# --- Auth ---
# generated by `docker compose run --rm setup`
SESSION_SECRET=
ADMIN_INITIAL_USERNAME=admin
ADMIN_INITIAL_PASSWORD=changeme-set-on-first-boot

# --- Vision LLM ---
GEMINI_API_KEY=
# may be overridden in-app; see "AI model resilience"
GEMINI_MODEL=gemini-3.6-flash
# optional segmentation model (e.g. gemini-2.5-flash); empty disables
# background removal (09-consumption-logging.md)
GEMINI_IMAGE_MODEL=

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

**The optional image model** (`GEMINI_IMAGE_MODEL`) — one that returns
segmentation masks, which background removal cuts along without the model
ever redrawing the picture (`09-consumption-logging.md`) — follows the
same rules, with one difference: it is **optional by design**. An empty or
unavailable value is not a degraded state — the background-removal offer
in `09-consumption-logging.md` simply does not appear, and nothing else in
the system is affected. It is listed in the admin banner only when it is
configured *and* missing from `models.list`, so a deployment that never
wanted the feature is never told anything is wrong.

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
    env_file:
      - path: .env
        required: false   # setup writes it; see "First-run order"
    depends_on:
      db:
        condition: service_healthy
    labels:
      - traefik.enable=true
      - traefik.http.routers.app.rule=PathPrefix(`/`)
      - traefik.http.services.app.loadbalancer.server.port=8000
    volumes:
      - uploads:/data/uploads
      # The suggestion-image cache (07-shopping-list-reconciliation.md). A
      # separate volume from uploads because the tiers have opposite
      # lifetimes: this one is evictable and re-fetchable by definition,
      # a promoted product image is permanent. Without a volume the cache
      # lives in the container's writable layer and is discarded on every
      # deploy, turning "one provider call per product, ever" into one per
      # product per release.
      - imagecache:/data/cache
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
    env_file:
      - path: .env
        required: false
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
  imagecache:
```

The single `app` service serves everything: the static frontend at `/`,
the JSON API at `/api`, and the server-rendered admin UI at `/admin`.
Traefik is the only ingress, which is what makes the remote-access layer
swappable (below) — except on the operator's own NAS, where Traefik is switched
off and a Tailscale sidecar is the ingress (see "Synology NAS variant").

## `docker-compose.override.yml` (local staging overrides)

Compose loads this file automatically, so a bare `docker compose up` /
`docker compose run` on the operator's PC with Docker Desktop is already the
dev stack. That auto-loading is deliberate — it is what puts a Go-capable image
under the `app` name so the documented `go test` and `go vet` commands work
against a project whose production image is `FROM scratch`.

The cost is that **production must pin the base file explicitly**
(`docker compose -f docker-compose.yml up -d`, see "Deployment model"); a bare
`up -d` on the NAS would otherwise inherit `APP_ENV=dev` and the published
port. The dev service also carries its own `image:` tag, because both targets
would otherwise export to `inventory-app:latest` and whichever was built last
would silently win.

```yaml
services:
  app:
    build:
      context: .
      target: dev
    image: inventory-app-dev    # distinct tag; see above
    environment:
      - APP_ENV=dev
      - STATIC_DIR=/src/web/static
    volumes:
      - ./web:/src/web          # edit frontend files, just refresh the browser
      - ./internal:/src/internal
      - ./cmd:/src/cmd
      # Without this, /src/migrations is whatever was baked into the image,
      # so a newly added migration is invisible to
      # `docker compose run --rm app go test ./...` until someone rebuilds —
      # and the symptom is a confusing "relation does not exist" from a
      # suite that migrates its own throwaway database.
      - ./migrations:/src/migrations
    ports:
      - "8000:8000"             # direct access, bypassing Traefik, for debugging
```

## Deployment model

- **Staging:** operator's PC, Docker Desktop, dev compose overrides,
  `APP_ENV=dev`, frontend served from disk.
- **Production:** Synology NAS, Container Manager, base `docker-compose.yml`
  only (on the operator's own NAS: the base plus one layer, both files pinned on
  every call — see "Synology NAS variant") — and that has to be
  stated on the command line, because Compose
  auto-loads `docker-compose.override.yml` when it is present. Because the
  Dockerfile performs the whole build, deploying is:

  ```console
  $ docker compose -f docker-compose.yml build
  $ docker compose -f docker-compose.yml up -d
  ```

  Building elsewhere and pulling from a registry is equally supported and
  requires no additional tooling. **The `-f docker-compose.yml` pin is a
  security control, not a style preference:** without it the NAS runs the
  `dev` target, which sets `APP_ENV=dev` and therefore emits `debug_reason`
  in error responses (`03-auth-and-multi-tenancy.md`), ships a toolchain image
  instead of the `scratch` binary, and publishes port 8000 past Traefik — on a
  Tailscale-only host, that is reachable to the whole tailnet without the
  ingress. A deployment checklist that omits the pin is a broken deployment.
  The Synology variant keeps this guarantee by pinning **two** files on every
  call instead of one; on that NAS `-f docker-compose.yml` alone is the wrong
  command (it starts Traefik on DSM's port 80).
- **Deployment gate:** the end-to-end browser suite
  (`05-frontend-pwa-foundations.md`) must pass before an image is
  promoted to production. Unit tests already run inside the image build;
  E2E runs separately because it needs a live stack. A deployment that
  skips it is not a valid deployment.

## Remote access (interchangeable by configuration)

Traefik is the sole ingress and terminates all internal routing (except on the
operator's own NAS, which replaces it with a Tailscale sidecar — see "Synology
NAS variant"). The
remote-access layer attaches to Traefik's `web` entrypoint and is therefore
a **drop-in, swappable component: changing it is a compose/config edit with
zero application changes.** Whichever is active, application-level session
auth (`03-auth-and-multi-tenancy.md`) still applies — network access
control is never a substitute for it.

- **Active default — Tailscale:** installed as a Synology package, joining
  the NAS to a private WireGuard network. No router ports are opened; the
  app is reached at the NAS's Tailscale address. Nothing in the
  application is Tailscale-specific. (The operator's own NAS runs it as a
  sidecar container instead, in place of Traefik — see "Synology NAS
  variant" below.)
- **Documented alternative — Cloudflare Tunnel:** if Tailscale proves
  impractical, add a `cloudflared` service to the compose file pointing at
  `http://traefik:80` and disable/ignore the Tailscale package. Keep this
  service present but commented out in `docker-compose.yml`, with a note
  that exactly one remote-access method should be active at a time.

Because the app must work on a LAN-only / Tailscale-only NAS with no
inbound internet exposure, the frontend must not depend on any CDN at
runtime — see the vendoring rule in `11-reporting-and-analytics.md`.

## Synology NAS variant: Tailscale sidecar, bind-mounted data

The operator's NAS (a DS923+ running Container Manager) deviates from the
general design above in three deliberate ways: (1) runtime data lives in bind
mounts inside the clone instead of named volumes, (2) Traefik is switched off,
and (3) Tailscale runs as a sidecar container that publishes the app, instead of
as a Synology package in front of Traefik. All three are one tracked file,
[`docker-compose.nas.yml`](../../docker-compose.nas.yml), layered on top of the
unchanged base file — so the clone on the NAS never carries a local edit, and
`git pull` has nothing to trip over.

**Selecting it: two files, pinned on every call.** Every command on the NAS is
`docker-compose` (with the hyphen, see "Compose version" below) followed by the
same prefix, written out in full. In an SSH session on the NAS, in the clone's
folder, set it once and use it as `$DC` (the variable is gone after a logout, so
set it again after every login):

```console
$ cd /volume1/docker/inventory        # the clone; adjust the path
$ DC="docker-compose -p inventory -f docker-compose.yml -f docker-compose.nas.yml"
```

To have it in every login shell instead, run `sh deploy/synology/install-shell`
once: it writes a marked block into `~/.profile` that exports `DC` and defines the
alias `dc`, with absolute paths, so both work from any directory (see
[`deploy/synology/README.md`](../../deploy/synology/README.md)).

The rest of this section writes `$DC …` for that command. The explicit `-f` pair
is the pin that "Deployment model" calls a security control: it keeps
`docker-compose.override.yml` (`APP_ENV=dev`, `debug_reason` in error responses,
port 8000 published past the ingress) out of the NAS stack. The pin is spelled
out on every call, and not kept in the environment, on purpose. An earlier design
selected the layer with `COMPOSE_FILE` in the clone's untracked `.env` and failed
open: the setup wizard rewrites `.env` from `.env.example`, dropping every line
that is not in the template, and the command it then prints
(`docker compose up -d --force-recreate`) has no `-f`, so it would load the dev
override and start a dev-flavoured stack on fresh named volumes next to the real
data in `./pgdata`. The project name (`-p inventory`) is fixed for the same
reason, and so that container, network and volume names stay the same wherever
the clone lives.

[`deploy/synology/compose`](../../deploy/synology/compose) is the same prefix as
a script — `deploy/synology/compose up -d` instead of `$DC up -d` — and is
optional. It exists only in a clone that has pulled `main` since it was added, it
must keep its executable bit (otherwise start it as `sh deploy/synology/compose
…`), and it needs LF line endings (a CRLF checkout breaks its first line;
`.gitattributes` pins that for the repository, not for a copy made by hand). If
it does not run, use `$DC`: nothing in this section depends on the script.

Consequently, on the NAS:

- **Never run a bare `docker-compose up`** — or `docker compose`, or `-f
  docker-compose.yml` alone — in the clone. Without the layer the base file
  starts Traefik, which asks for host port 80 that DSM's own web server already
  holds (`Bind for 0.0.0.0:80 failed: port is already allocated`), and without
  any `-f` the dev override is merged as well. The `-f docker-compose.yml`
  commands elsewhere in this document and in the README are for a plain clone,
  not for this NAS.
- **Do not copy the command the setup wizard prints when it finishes** (`docker
  compose up -d`); use `$DC up -d`.
- **Never put a `compose.yml` (or `compose.yaml`) into the clone.** Compose
  prefers those names over `docker-compose.yml` and silently ignores the latter
  (it prints only a warning), so a private copy would replace the repository's
  file for any command that does not pin its files with `-f`.
- Check before starting anything: `$DC config --services` must list `app`, `db`
  and `ts-inventory` and must not list `traefik`.

**Ingress: a Tailscale sidecar instead of Traefik.** `ts-inventory` publishes
the app on the tailnet over HTTPS (`https://inventory.<tailnet>.ts.net`) by
proxying to `http://app:8000` through `tailscale serve`, configured by
[`deploy/synology/tailscale/serve.json`](../../deploy/synology/tailscale/serve.json)
via `TS_SERVE_CONFIG`. (`tailscale up` has no `--serve` flag, so that cannot be
passed through `TS_EXTRA_ARGS`.) The `${TS_CERT_DOMAIN}` in that file is expanded
by the Tailscale container at start-up to the node's MagicDNS name; it is not a
Compose variable, and must not be replaced with a tailnet name. Traefik is
disabled by an inactive profile, so
the stack publishes no host port at all: nothing can clash with DSM's own
80/443/5000/5001, and the Docker socket is no longer mounted into a container.

- The tailnet needs **MagicDNS and HTTPS certificates** enabled. HTTPS is not
  optional: the session cookie is `Secure` (`03-auth-and-multi-tenancy.md`), so
  plain HTTP would log no one in.
- The sidecar joins the app's network namespace (`network_mode: service:app`,
  the same pattern as the operator's other Tailscale sidecars) and, like
  those, gets `NET_ADMIN` and the `/dev/net/tun` device. Both are inherited,
  not required: the image defaults to userspace networking, which is all
  `tailscale serve` needs, and `TS_USERSPACE` is not changed, so they could be
  dropped without effect (`NET_ADMIN` inside the app's network namespace is also
  more privilege than the sidecar uses). The device must exist on the NAS: for a
  missing path a `volumes:` bind mount makes Docker create a directory in its
  place instead of failing.
- **Restart `ts-inventory` whenever `app` is restarted on its own.** The
  sidecar keeps the network namespace of the app container it started
  against and cannot reach a restarted one until it is restarted too. That
  includes Docker's own automatic restart of `app` after a crash: nothing
  restarts the sidecar then, so recovery is manual —
  `$DC restart ts-inventory`. `up -d` recreates both, so the
  update flow below needs no extra step. (Giving the sidecar its own network and
  proxying to `http://app:8000`, which `serve.json` already does, would remove
  the coupling; the operator chose the shared namespace to match their other
  stacks.)
- **The auth key is passed once, on the command line, and never written to a
  tracked file:** `TS_AUTHKEY=tskey-auth-… $DC up -d`. It
  still lands in the shell history and, through interpolation, in the
  container's configuration (`docker inspect`, the Container Manager UI) until
  the container is recreated. Use a one-off key with a short expiry, and revoke
  it if the first start fails. The node identity then persists in
  `./ts_inventory_state`, which holds the node key and is treated like a secret.

**Data in the clone.** `./pgdata`, `./uploads` and `./imagecache` are bind
mounts inside the clone instead of named volumes, so they are visible in File
Station, and `./backups` joins them as the directory the `backup` service
writes archives into (`15-backup-restore-and-export.md`). All four are
gitignored and dockerignored, and both matter: without the
`.dockerignore` entries the build context would contain the live Postgres data
directory (files the context reader cannot always read, a copy of the database
in every `COPY . .` layer, gigabytes per build). Two consequences to keep in
mind:

- `git clean -x` / `-X` in that clone deletes all of it — **including
  `./backups`**, where the `backup` service writes its archives
  (`15-backup-restore-and-export.md`). Do not run it there. Deleting the live
  data and the backups of it in one command is the one mistake this section
  exists to prevent.
- A file-level copy of a *running* Postgres data directory — Hyper Backup
  included — is not a consistent backup. Back the instance up with
  `run --rm backup` (`15-backup-restore-and-export.md`), which takes a logical
  `pg_dump` and the `uploads` tree in one archive.

**Compose version.** The files use the long-form `env_file` with
`required: false`, which needs **Compose ≥ 2.24** (see "Minimum Docker Compose
version" above). Container Manager bundles v2.20.1, which rejects the whole
file. Install a newer standalone binary in a shared folder — one a DSM update
does not overwrite — and put it first on `PATH` (v2.31.0 is what the development
machines run). The commands here assume a **root shell** (`sudo -i`): a plain
`sudo <command>` may reset `PATH`, find the bundled v2.20.1 again and fail with a
parse error, and may not pass on a `TS_AUTHKEY=…` prefix. For root, make the
`PATH` line permanent in `/root/.profile`.

```console
$ mkdir -p /volume1/docker/bin && cd /volume1/docker/bin
$ curl -fLO https://github.com/docker/compose/releases/download/v2.31.0/docker-compose-linux-x86_64
$ curl -fLO https://github.com/docker/compose/releases/download/v2.31.0/docker-compose-linux-x86_64.sha256
$ sha256sum -c docker-compose-linux-x86_64.sha256
$ chmod +x docker-compose-linux-x86_64 && mv docker-compose-linux-x86_64 docker-compose
$ export PATH=/volume1/docker/bin:$PATH
```

Use `docker-compose …` (with the hyphen) on the NAS: a `docker compose` plugin,
if present, is the bundled old version. Container Manager's Project tab uses
that same bundled Compose and cannot load these files, so the stack is operated
over SSH, and the UI is only good for looking at running containers.

**First start.** The stack does not migrate on its own, and since
[`18-operations-and-observability.md`](18-operations-and-observability.md) it
does not pretend to either: without `migrate up` the app refuses to start,
naming the command, and exits with the same non-retryable code as the
missing-`.env` check. (It used to start and then log
`relation "jobs" does not exist` from every background sweep while serving
nothing useful.) The bind-mounted `./pgdata` starts empty, so this applies to
every new clone.

```console
$ cd /volume1/docker/inventory        # the clone; adjust the path
$ DC="docker-compose -p inventory -f docker-compose.yml -f docker-compose.nas.yml"
$ $DC run --rm setup           # writes .env
$ $DC build
$ $DC run --rm app migrate up  # also creates the initial admin
$ TS_AUTHKEY=tskey-auth-… $DC up -d
```

The wizard ends by printing `docker compose up -d`; ignore it and use `$DC up -d`.
`sh deploy/synology/update` runs the build, migrate and start of this sequence for
you when no app instance is running (mind the `TS_AUTHKEY` note under "Updating").

**Updating: `sh deploy/synology/update`.** The upgrade rules of
[`18-operations-and-observability.md`](18-operations-and-observability.md)
("Upgrades") apply: **back up first**, and migrations only go forward. The script
takes no backup and cannot undo a migration. What it does is update so that a
release that does not start never replaces one that does. It pulls the code and
the sidecar image, builds, runs `migrate up`, waits until no job is pending, starts
a **second** app instance from the new image next to the old one, waits until that
one answers `GET /healthz`, waits once more until no job is pending, then stops and
removes the old one and recreates the sidecar. Its contract:

- **The old instance is not touched until the new one is healthy.** A failed
  build, migration or start leaves the old app serving; a new instance that fails
  to start, does not become healthy, or is interrupted is removed again, and no
  stopped instance is left behind. The database stays migrated in every one of
  those cases.
- **Both `-f` files on every compose call**, and it refuses to run if the merged
  model — checked after `git pull`, on the files it will use — lists `traefik` or
  lacks `app`, `db` or `ts-inventory` (the NAS layer is not active), or if Compose
  is older than 2.24.
- **It refuses, before it pulls anything,** when more than one app instance runs or
  a stopped one is left over, and it runs one at a time (a lock directory). It also
  refuses to pull into a clone that has local changes to tracked files: what is
  deployed is what was pulled (`--no-pull` deploys the tree as it is).
- **`--classic`** stops the app and the sidecar first, then migrates, then starts:
  for a release whose migration the previous release cannot run against. If the
  migration fails, the stack stays stopped, and the script says so.
- **Nothing to do** is detected by asking Compose whether it would recreate, create
  or start anything (`up --dry-run`); if not, nothing is touched.

What it does not give you, and the reasons:

- **Not zero downtime.** The sidecar shares the app's network namespace and must be
  recreated with it: a few seconds without the tailnet URL. While the new instance
  starts, the name `app` also resolves to both instances, so a request can reach the
  new one before its health check has passed.
- **Two releases overlap:** the migration runs against the old app, and the old app
  runs on the migrated schema until it is retired.
- **The app assumes a single process.** `FailInterruptedJobs` marks every pending
  job failed on start (`04-backend-api-conventions.md`), and a normal stop cancels
  the jobs the app is running and fails them as interrupted. So a second instance can
  fail a job the first is still working on, and stopping the first can fail a job
  submitted to it during the overlap. The script waits for pending jobs to finish
  before it starts the second instance and again before it stops the old one; a job
  submitted in the seconds after either check is still failed, and the user re-runs
  the analysis. Closing that needs the recovery to be aware of which process owns a
  job, which is application work, not deployment (issue #121).
- **No way back without the backup.** Going back to an earlier release after a
  migration means restoring the backup (`18`); an old binary on a newer schema is a
  fatal start-up error there.

Two operational notes. The app container is **no longer named `inventory_app`**: a
fixed name would forbid the second instance, so Compose names them
`inventory-app-<n>`. And the very first start of the sidecar needs the one-off key:
`TS_AUTHKEY=tskey-auth-… sh deploy/synology/update --no-pull`, or the sidecar starts
without joining the tailnet and the script still exits 0.

Options, environment, the Task Scheduler command and a checklist for verifying a
change to the script are in
[`deploy/synology/README.md`](../../deploy/synology/README.md). Without the script,
the same update by hand is `git pull`, then `$DC pull ts-inventory`, `$DC build`,
`$DC run --rm app migrate up`, `$DC up -d`, which stops the app before it starts
the new one.

## Health/readiness

`app` exposes `GET /healthz` returning `200` once it can reach the
database, with the vision-subsystem status described under AI model
resilience. Used by the compose healthcheck pattern and by Traefik.
