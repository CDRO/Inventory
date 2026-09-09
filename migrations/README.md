# Migrations

goose SQL migrations, applied only through Docker:

```bash
docker compose -f docker-compose.yml run --rm app migrate up
docker compose -f docker-compose.yml run --rm app migrate status
```

Two details in that command are load-bearing:

- **`-f docker-compose.yml` is required.** `migrate` is a subcommand of the
  compiled `/inventory` binary, which only the production image carries.
  Without the pin, Compose auto-loads `docker-compose.override.yml` and `app`
  resolves to the Go dev image, which has no such binary.
- **The binary is not named again.** The image sets
  `ENTRYPOINT ["/inventory"]`, so `... app /inventory migrate up` would run
  `/inventory /inventory migrate up` and fail with
  `unknown command "/inventory"`.

## What is here

| File | Contents |
|---|---|
| `00001_extensions.sql` | `pg_trgm`, alone and first — the `gin_trgm_ops` indexes in the next migration cannot be declared without it |
| `00002_core_schema.sql` | The core tables of [`docs/specs/02-data-model.md`](../docs/specs/02-data-model.md) |
| `00003_client_sync.sql` | `pairing_codes`, `idempotency_records`, `tombstones` — the client contract in [`docs/specs/12-client-api-contract.md`](../docs/specs/12-client-api-contract.md) |

Every file carries both `-- +goose Up` and `-- +goose Down`, and the down path
is exercised in CI-equivalent form: `down` three times empties the schema and
`up` restores all fifteen tables.

`00001`'s down step deliberately does **not** drop the extension. Other schemas
in the same database may depend on `pg_trgm`, and dropping it would take their
indexes with it.

## Adding a migration

Number it in sequence and keep it forward-only once merged — a migration that
has run on the NAS must never be edited, only superseded. The application
supplies every `id` as a UUIDv7 (`uuid.NewV7`), so new tables declare
`id UUID PRIMARY KEY` with **no** default: a missing id has to fail loudly
rather than fall back to a random v4 that breaks index locality.

Note that `migrate status` currently prints nothing — goose reports it through
a logger the application sets to a no-op. Tracked separately; `migrate up`
itself is unaffected.
