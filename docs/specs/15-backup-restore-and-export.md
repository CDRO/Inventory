# 15 — Backup, Restore & Data Export

Depends on: [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md)
(volumes, no-host-toolchain rule, compose contexts),
[`02-data-model.md`](02-data-model.md),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md) (image
storage areas), [`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(the cache tier that is deliberately *not* backed up).

## Why this spec exists

The system's entire pitch is "your data, on your NAS." A NAS disk dies
like any other disk, and until now the spec set has not contained the
word *backup*. This spec defines two different guarantees for two
different audiences:

- **Operator backup/restore** — the whole instance, all storages, for
  disaster recovery and for moving to new hardware.
- **Member export** — one storage, as portable files, for "my data is
  mine" — readable without this software.

Neither may violate the no-host-toolchain rule: everything runs through
`docker compose`, nothing assumes `pg_dump` or even `tar` on the host.

## What must be backed up, and what must not

| Data | Where | In backup? | Why |
|---|---|---|---|
| Database | `pgdata` volume | **Yes** — as a logical dump | Everything: inventory, users, logs, settings, catalog |
| Uploaded/product images | `uploads` volume | **Yes** | Irreplaceable — photos of the user's own shelves and products |
| Suggestion image cache | `imagecache` volume | **No** | Re-fetchable by definition (`07-shopping-list-reconciliation.md`); backing it up wastes the largest share of bytes on the most replaceable data |
| `.env` | project dir on the host | **No** — operator keeps it separately | It holds secrets (API keys, `SESSION_SECRET`); a backup archive travels, and secrets should not travel with it. The restore procedure documents regenerating it via `docker compose run --rm setup` and which values must match (below) |

The dump is **logical** (`pg_dump`), not a copy of `pgdata` files: a file
copy of a running database is corrupt by default, and a logical dump
restores across PostgreSQL minor/major versions, which matters when the
restore target is a new NAS years later.

## The `backup` service

A `backup` service is added to `docker-compose.yml` under the `tools`
profile (like `setup`), using the same `postgres:16-alpine` image as
`db` — it brings `pg_dump` and BusyBox `tar`, so no new image and nothing
on the host:

```console
$ docker compose -f docker-compose.yml run --rm backup
```

- Mounts `uploads` read-only and a bind mount `./backups:/backups`.
- Runs `pg_dump` against the `db` service (credentials from `.env` via
  the existing `env_file` mechanism), writes `db.sql`, then packs
  `db.sql` plus the `uploads` tree into
  `/backups/inventory-backup-<YYYY-MM-DD-HHMM>.tar.gz`.
- Exits non-zero, with the reason on stderr, if the dump fails or the
  archive cannot be written — a cron-driven backup must fail loudly, not
  produce a silent empty file. On success it prints the archive path and
  size.
- The base-file pin matters here as everywhere
  (`01-architecture-and-deployment.md`): the command must work
  identically on the NAS, where only the base file exists.

Scheduling is the operator's (DSM Task Scheduler runs exactly this
command); the application does not schedule its own backups, for the
same reason it does not restart its own containers.

## Restore procedure

Documented in the repository (`docs/` runbook or README section written
as part of this spec's implementation), tested by the acceptance
criteria, and deliberately boring:

1. Fresh clone; `docker compose run --rm setup` to write a new `.env`.
   `SESSION_SECRET` may be freshly generated — all sessions die at
   restore, which is correct. `POSTGRES_*` values must match what the
   dump expects only insofar as `setup`'s defaults do; the restore step
   below creates the role/database from `.env` before loading.
2. Start only the database: `docker compose -f docker-compose.yml up -d db`.
3. Load the dump and unpack uploads via the same `backup` image, in
   restore mode:
   `docker compose -f docker-compose.yml run --rm backup restore /backups/<archive>`
   — drops and recreates the application database, loads `db.sql`,
   unpacks `uploads/` into the volume. Refuses to run if the `app`
   service is up (a running app mid-restore corrupts both).
4. `docker compose -f docker-compose.yml run --rm app migrate up` — the
   dump carries the goose version table, so this applies only migrations
   newer than the backup, and is a no-op when versions match.
5. `docker compose -f docker-compose.yml up -d`.

**The image cache after a restore:** `cached_images` rows are in the
dump but their files were deliberately not archived. The serving handler
must therefore treat a missing cache file as a cache miss: delete the
row and proceed as if it had never existed (the fetch-or-fallback path
from `07-shopping-list-reconciliation.md`). This complements the
existing orphan-file sweep — together the cache self-heals in both
directions, and a restore needs no cache-specific step.

## Member export — one storage as portable files

`GET /api/storages/{storage_id}/export` — storage-scoped behind
`RequireStorageMember` like everything else; any member may export, since
rights are flat (`03-auth-and-multi-tenancy.md`).

Streams a ZIP (Go `archive/zip`, stdlib) named
`inventory-export-<storagename-slug>-<YYYY-MM-DD>.zip`:

- `export.json` — `{"format": "inventory-export/1", "exported_at": …,
  "storage": {name}, "locations": […], "categories": […],
  "products": […], "batches": […], "logs": […], "shopping_lists": […]}`.
  Trees are exported nested, like their GET endpoints. Products include
  `current_stock`. Log rows resolve `created_by` to a **display name
  string** — user ids are meaningless outside this instance.
- `images/` — the permanent product images this storage's products
  reference (`/data/uploads/products/`), named by product so the JSON
  can reference them by relative path.

What it must **never** contain: any other storage's data, anything from
`catalog_products` beyond what is already denormalized onto this
storage's own products, password hashes, session tokens, `is_admin`, or
internal file paths. Images were EXIF-stripped at upload
(`04-backend-api-conventions.md`), so the archive carries no location
metadata — this is worth an explicit test, because an export is the one
artifact designed to leave the house.

**There is no import endpoint, deliberately.** Whole-instance moves go
through backup/restore above; the export exists so the data outlives the
software, not as a sync format. A selective import is future work with
its own spec if it is ever actually wanted.

## Acceptance criteria

- A backup archive contains the SQL dump and the uploads tree, and
  nothing from `imagecache`; `.env` is absent.
- `run --rm backup` exits non-zero and prints a reason when the database
  is unreachable; it never writes a partial archive under the final
  filename (write to a temp name, rename on success).
- The documented restore procedure, executed on a clean checkout with
  only Docker present, yields a working stack where every storage's
  inventory, images, users, and settings match the backed-up instance;
  all prior sessions are invalid.
- After a restore, a product image displays correctly, and a suggestion
  image whose cache file is gone is silently re-fetched (or falls back),
  with its stale `cached_images` row removed — never a broken image with
  a dangling row.
- An export ZIP for storage A, requested by a member of A, contains no
  identifier or datum from any other storage, no credential material,
  and images free of EXIF/GPS metadata.
- A non-member requesting the export gets the standard `404`
  (`03-auth-and-multi-tenancy.md`).
- `export.json` parses, and its `format` field is
  `inventory-export/1`; additive changes bump the suffix.
