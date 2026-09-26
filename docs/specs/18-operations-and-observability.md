# 18 — Operations: Logging, Admin Audit Trail, Upgrades

Depends on: [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md),
[`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md),
[`15-backup-restore-and-export.md`](15-backup-restore-and-export.md)
(backup is the rollback strategy).

## Why this spec exists

`04-backend-api-conventions.md` names a "request logging" middleware and
`03-auth-and-multi-tenancy.md` says dev "logs the reason at info level" —
but nothing defines the log format, what levels mean, or, critically,
what must **never** be logged. Likewise the admin area can create users
and delete catalog entries with no record of who did what, and the
upgrade path ("new image, then what?") exists only as folklore. For a
system operated by one person on a NAS, `docker compose logs app` *is*
the observability stack — so what lands there is worth specifying.

## Structured logging

All application logging goes through Go's `log/slog` with the JSON
handler, to **stdout** — Docker's log driver does retention and
rotation; the app writes no log files.

Every request, on completion, logs one line at `info`:

```json
{"time":"…","level":"INFO","msg":"request","request_id":"018f…",
 "method":"POST","path":"/api/storages/{storage_id}/consume/photos",
 "status":202,"duration_ms":41,"user_id":"018f…","storage_id":"018f…"}
```

- **`request_id`** — a UUIDv7 minted by middleware for every request,
  attached to the context, echoed as an `X-Request-Id` response header,
  and included in every log line emitted while handling that request.
  It is what turns "it failed around noon" into a traceable incident.
- **`path` is the route pattern**, not the raw URL — the chi route
  template. Raw URLs would fill logs with UUIDs that are only noise, and
  query strings can carry user text.
- `user_id`/`storage_id` appear when resolved; they are UUIDs and leak
  nothing (`02-data-model.md`).
- Levels: `info` for requests and lifecycle events (startup, migration
  check, sweep results); `warn` for degraded-but-handled situations
  (vision model unavailable, notification delivery failure, upstream
  image fetch failed); `error` for 5xx responses and failed background
  jobs, with the Go error string.

**Never logged, at any level, in any environment:** session tokens or
cookies, pairing codes, passwords or hashes, `Idempotency-Key` values,
API keys, request/response bodies, uploaded image bytes or their
filesystem paths joined with user text, and — in production —
`debug_reason` (which stays response-only in dev and server-side-log-only
in prod exactly as `03-auth-and-multi-tenancy.md` already rules).
External calls (Gemini, SerpAPI, Iconify, notification targets) log the
provider, duration, and status — never the prompt, the image, or the
response payload.

## Admin audit trail

Every mutating admin action is recorded — because admin actions cross
tenant boundaries (user creation, membership grants, catalog
moderation), they are exactly the actions that must be reconstructable
after the fact.

```sql
CREATE TABLE admin_audit_log (
    id         UUID PRIMARY KEY,
    actor_id   UUID REFERENCES users(id) ON DELETE SET NULL,
    action     TEXT NOT NULL,     -- e.g. 'user_created', 'storage_member_added',
                                  -- 'catalog_entry_deleted', 'settings_updated',
                                  -- 'user_password_reset'
    target     TEXT,              -- the affected id/key, as text
    details    JSONB,             -- action-specific, never secrets
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- Written in the **same transaction** as the admin mutation, from the
  admin handlers — one call site per action, mirroring how the error
  serializer is centralized (`04-backend-api-conventions.md`).
- Append-only: no update or delete path exists in the application.
- `details` carries facts like a created username or a granted
  storage id — never a password, even transiently.
- Viewable as a server-rendered table at `/admin/audit` (newest first,
  simple pagination), behind the same `RequireSession → RequireAdmin`
  chain as every admin page. It is never exposed through any
  non-admin route, and its existence is hidden like the rest of the
  admin area (`404`).

Storage-scoped user actions are deliberately **not** audited here —
`inventory_logs` already records who changed quantities, which is the
household-level accountability that matters. This table is for the
operator surface only.

## Version visibility

- The binary embeds a version string at build time
  (`-ldflags "-X main.version=…"`, passed as a Docker build arg;
  `dev` when unset). The Dockerfile change is additive and does not
  disturb the reproducible single-file build
  (`01-architecture-and-deployment.md`).
- `GET /healthz` gains `"version": "…"` beside the existing fields, and
  the admin UI footer shows it — so "what is the NAS actually running"
  is answerable without SSH.

## Upgrades

The upgrade procedure, in order, on the NAS:

1. **Back up** (`15-backup-restore-and-export.md`). The backup is the
   rollback: migrations are **forward-only** — no `migrate down` in
   production, because down-migrations against real data are tested
   never and trusted always. Rolling back a bad upgrade means restoring
   the backup taken in this step.
2. `docker compose -f docker-compose.yml build` (or pull).
3. `docker compose -f docker-compose.yml run --rm app migrate up`.
4. `docker compose -f docker-compose.yml up -d`.

On the operator's Synology NAS, steps 2 to 4 are `sh deploy/synology/update`, which
brings the second app instance up beside the old one before it retires that one
(`01-architecture-and-deployment.md`, "Synology NAS variant"). Step 1 is still
yours: the script takes no backup, and going back after a migration still means
restoring it.

To make skipping step 3 loud instead of weird: **on startup, `serve`
compares the database's goose version against the migrations the binary
ships.** If migrations are pending, it exits fatally — same
non-retryable pattern as the missing-`.env` check in
`01-architecture-and-deployment.md` — naming the fix:

```
Database schema is 5 migrations behind this binary.
Run:  apply the pending migrations (migrate up) the way you deploy it (see README.md).
Then: start the stack the same way.
```

Neither line names a compose invocation, and each is wrong on the operator's
own Synology NAS variant for a different reason: `docker compose -f
docker-compose.yml run --rm app migrate up` (missing the NAS's second `-f`
layer) resolves `db` to a throwaway named volume instead of the NAS's
bind-mounted `./pgdata`, and reports success while the real database stays
untouched (`migrations/README.md`); `docker compose -f docker-compose.yml up
-d` starts Traefik, which is wrong because DSM already holds port 80
(`01-architecture-and-deployment.md`, "Synology NAS variant"). README.md
names the right two commands for every variant.

A database **newer** than the binary (a restore of a newer dump, or a
rolled-back image) is likewise a fatal, named error rather than
undefined behavior against unknown columns.

## Acceptance criteria

- Every API request produces exactly one completion log line with
  `request_id`, route pattern, status, and duration; the same
  `request_id` is on the `X-Request-Id` header and on every other line
  logged during that request.
- Grepping a production log capture for a live session token, a
  pairing code, a password, or an API key finds nothing — covered by a
  test that drives login/pairing/upload and asserts over captured log
  output.
- Every route in the admin table (`03-auth-and-multi-tenancy.md`) that
  mutates writes exactly one `admin_audit_log` row in the same
  transaction, and `/admin/audit` renders it; a non-admin gets `404`
  for `/admin/audit` like any admin page.
- `admin_audit_log.details` never contains password material — asserted
  for user-create and password-reset actions.
- `/healthz` reports the built version; a dev build reports `dev`.
- `serve` refuses to start, with the documented message and a non-zero
  exit, when migrations are pending or the schema is newer than the
  binary; `restart: unless-stopped` does not loop it into log spam
  (same distinct-exit behavior as the config check in
  `01-architecture-and-deployment.md`).
