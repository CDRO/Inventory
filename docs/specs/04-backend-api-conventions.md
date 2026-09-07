# 04 — Backend API Conventions

Depends on: [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md),
[`02-data-model.md`](02-data-model.md), [`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md).

## Project structure

```
cmd/inventory/main.go     # subcommands: serve (default), setup, migrate
internal/
├── config/               # env loading + settings-table overrides (settings wins)
├── httpapi/              # chi router, JSON handlers, middleware
│   ├── router.go          # route registration
│   ├── middleware.go      # session lookup, storage scoping, request logging
│   ├── errors.go          # THE single error-envelope serializer (see below)
│   └── *_handler.go       # one file per resource
├── admin/                # server-rendered admin handlers (html/template)
├── store/                # pgx queries, one file per table group
├── vision/               # Gemini client, prompts, parsing, model resilience
├── imagesearch/          # SerpAPI + Iconify clients
├── matching/             # shared product matching (catalog-first, then trigram)
├── jobs/                 # background job runner + job store
└── export/               # CSV/PDF generation
web/
├── static/               # vanilla frontend, embedded via embed.FS
└── templates/            # admin UI templates
```

Handlers stay thin: parse/validate input, call a `store`/service function,
serialize. Business rules live in `internal/*` packages so they are
testable without HTTP.

## Route conventions

- `/api/auth/*` — session lifecycle (`03-auth-and-multi-tenancy.md`).
- `/api/admin/*` — admin JSON routes. **Mounted behind
  `RequireSession` → `RequireAdmin`.**
- `/admin/*` — server-rendered admin pages (`internal/admin`). **Mounted
  behind the exact same `RequireSession` → `RequireAdmin` chain.** The
  HTML routes are not a separate, weaker path: rendering an admin page
  goes through the identical middleware as the JSON routes that page
  posts to.
- `/api/storages/{storage_id}/*` — everything storage-scoped. Mounted
  behind `RequireSession` → `RequireStorageMember`.
- `/` and all other paths — the static frontend (embedded `web/static`, or
  read from `STATIC_DIR` when set). No session required.

### Middleware definitions

These three middlewares are the only places authorization is decided. No
handler performs its own check, and no check is duplicated inline.

**`RequireSession`**

1. Read the session cookie. Absent → `401 unauthorized`
   (`debug_reason: session_missing`).
2. Look up `sessions` by that id. Missing, or `expires_at <= now()` →
   delete the row if present, then `401` (`session_expired`).
3. Load the `users` row and place it in the request context.

**`RequireAdmin`** — always mounted after `RequireSession`

1. **Re-query `users.is_admin` from the database for the context user, on
   every single request.** Never read it from the session record, a cached
   user struct carried across requests, a cookie, a header, or any
   client-supplied value — by design there is no client-supplied value to
   read (`03-auth-and-multi-tenancy.md`).
2. `is_admin = FALSE` → respond `404 not_found`
   (`debug_reason: not_admin`), byte-identical in body and headers to any
   other `404`. The admin area must not disclose that it exists.
3. This applies identically to `/admin/*` HTML and `/api/admin/*` JSON.
   Register both groups on a single `chi` sub-router carrying this chain,
   so that adding a route to that group is what protects it and a new
   admin route cannot be forgotten.

**`RequireStorageMember`** — always mounted after `RequireSession`

1. Parse `{storage_id}` from the path; a malformed UUID → `404 not_found`.
2. Query `storage_members` for `(storage_id, user.id)`. No row — whether
   because the storage does not exist or because the caller is not a
   member → `404 not_found`. The two cases are distinguishable **only**
   via `debug_reason` (`storage_not_found` / `not_storage_member`) when
   `APP_ENV=dev`; the production response is identical for both.
3. Place the resolved storage id in the request context. Handlers read it
   from there and must never re-parse it from the URL, so a handler cannot
   accidentally operate on an id that was never validated.

## Response envelope

Success responses return the resource or collection directly, with the
appropriate 2xx status. Collection endpoints that can grow return:

```json
{ "items": [ ... ], "next_cursor": "opaque-string-or-null" }
```

## Error format

One shape for every error, produced by **exactly one serializer** in
`internal/httpapi/errors.go`:

```json
{ "error": { "code": "not_found", "message": "Not found." } }
```

- `code` is a stable machine-readable snake_case string the frontend can
  switch on; `message` is human-readable fallback text.
- Validation errors (`422`) add a `fields` map of field name → messages.
- `debug_reason` is appended **only** when `APP_ENV=dev`
  (`03-auth-and-multi-tenancy.md`). Because every error passes through
  this one function, a production build cannot leak it from a forgotten
  handler.

### Status code conventions

| Situation | Status | Code |
|---|---|---|
| No/expired session | 401 | `unauthorized` |
| Storage unknown **or** caller not a member | 404 | `not_found` |
| Admin route, caller not admin | 404 | `not_found` |
| Resource exists in the caller's storage but action is illegal (e.g. deleting a location that still holds inventory) | 409 | `conflict` |
| Payload validation failure | 422 | `validation_failed` |
| Upload exceeds size limit | 413 | `payload_too_large` |
| Configured Gemini model unavailable | 503 | `model_unavailable` |

`403` is deliberately unused for storage and admin scoping — see the
non-enumeration rules in `03-auth-and-multi-tenancy.md`.

## Pagination

Cursor-based, not offset-based (avoids drift under concurrent writes).
List endpoints accept `?limit=50&cursor=...` and return `next_cursor: null`
when exhausted. Default `limit` 50, max 200.

## File/image upload handling

Shelf photos can be up to 50MP (`06-vision-shelf-ingestion.md`):

- Uploads use `multipart/form-data`, never base64-in-JSON (which would add
  ~33% overhead on already-large images).
- Wrap the request body in `http.MaxBytesReader` with a hard cap (25MB
  compressed file size, independent of pixel count) before parsing;
  respond `413 payload_too_large` when exceeded. Use
  `r.ParseMultipartForm` with a bounded in-memory limit so large uploads
  spill to disk instead of into RAM — relevant on a NAS.
- Persist the original to the `uploads` volume (`/data/uploads`, separate
  from the Postgres volume) **before** calling the vision API, so a failed
  AI call does not lose the photo and the job can be retried.
- Store generated filenames (UUID + extension derived from the sniffed
  content type); never build a path from client-supplied filenames.

### Image storage areas

Three distinct areas under the `uploads` volume, with different lifetimes:

| Path | Holds | Lifetime |
|---|---|---|
| `/data/uploads/ingest/` | Photos awaiting or backing a review job | Until the job is `consumed` or discarded, then 30 days |
| `/data/uploads/products/` | The image finally chosen for a product | Permanent, until the product is deleted |
| `/data/cache/imagesearch/` | Fetched SerpAPI/Iconify suggestion images | Evictable; hard 1GB cap (`07-shopping-list-reconciliation.md`) |

No external image URL is ever handed to the browser: suggestions are
fetched server-side, cached, and served from our own origin
(`07-shopping-list-reconciliation.md`).

## Background jobs

Vision calls take seconds and must not block a request. There is no
external queue broker — at household scale, in-process goroutines plus a
`jobs` table are sufficient and survive a restart with visible state.

```sql
CREATE TABLE jobs (
    id           UUID PRIMARY KEY,       -- UUIDv7, returned to the client
    storage_id   UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL
                 CHECK (kind IN ('shelf_ingestion', 'product_photo',
                                 'shopping_list_photo', 'consumption_photo')),
    status       TEXT NOT NULL
                 CHECK (status IN ('pending', 'done', 'failed', 'consumed')),
    payload      JSONB,                  -- parsed proposal once done
    error        TEXT,
    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- The upload endpoint persists the image, inserts a `pending` job,
  launches a goroutine to do the vision call, and returns
  `202 {"job_id": "..."}` immediately.
- `GET /api/storages/{storage_id}/jobs/{job_id}` returns status and, once
  `done`, the proposal payload. The job is storage-scoped like everything
  else, so one storage's members cannot poll another's job.
- `GET /api/storages/{storage_id}/jobs?status=…` lists jobs for the review
  inbox, and `DELETE /api/storages/{storage_id}/jobs/{job_id}` discards
  one. Uploading and reviewing are decoupled: a `done` job waits
  indefinitely, is visible to every member of the storage, and is never
  auto-expired while unreviewed (`06-vision-shelf-ingestion.md`).
- On process restart, jobs left `pending` are marked `failed` with a
  retryable error at startup — never left hanging forever.
- A job moves to `consumed` when its confirm endpoint has been applied, so
  the same proposal cannot be committed twice.
- Frontend polls this endpoint (~1.5s interval) via the shared helper in
  `05-frontend-pwa-foundations.md`.

## Testing

`docker compose run --rm app go test ./...`, per the no-host-toolchain
constraint in `01-architecture-and-deployment.md`. Tests that need a
database run against a disposable database on the same `db` service (a
separate database name, created and dropped by the test setup) — never
against the production data in the `pgdata` volume. Prefer table-driven
tests for the pure logic in `matching`, `vision` (response parsing), and
`export`.
