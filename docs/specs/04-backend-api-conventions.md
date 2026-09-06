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
- `/api/admin/*` — admin JSON routes, gated by an `RequireAdmin`
  middleware that re-queries `users.is_admin` from the database on every
  request.
- `/admin/*` — server-rendered admin pages (`internal/admin`), same
  per-request DB check.
- `/api/storages/{storage_id}/*` — everything storage-scoped, gated by a
  `RequireStorageMember` middleware implementing the non-enumeration rules
  in `03-auth-and-multi-tenancy.md` and injecting the resolved storage id
  into the request context.
- `/` and all other paths — the static frontend (embedded `web/static`, or
  read from `STATIC_DIR` when set).

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

## Background jobs

Vision calls take seconds and must not block a request. There is no
external queue broker — at household scale, in-process goroutines plus a
`jobs` table are sufficient and survive a restart with visible state.

```sql
CREATE TABLE jobs (
    id           TEXT PRIMARY KEY,       -- opaque id returned to the client
    storage_id   INT NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    kind         VARCHAR(40) NOT NULL
                 CHECK (kind IN ('shelf_ingestion', 'shopping_list_photo', 'consumption_photo')),
    status       VARCHAR(20) NOT NULL
                 CHECK (status IN ('pending', 'done', 'failed', 'consumed')),
    payload      JSONB,                  -- parsed proposal once done
    error        TEXT,
    created_by   INT REFERENCES users(id) ON DELETE SET NULL,
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
