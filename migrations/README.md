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

The directory is empty on purpose. The schema is defined by
[`docs/specs/02-data-model.md`](../docs/specs/02-data-model.md) and lands with
that spec's issue; this spec ships the runner only, so `migrate up` reports
that there is nothing to apply rather than failing.

The first migration must enable the trigram extension the product matching
depends on:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```
