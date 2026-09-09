# Migrations

goose SQL migrations, applied only through Docker:

```bash
docker compose run --rm app /inventory migrate up
docker compose run --rm app /inventory migrate status
```

The directory is empty on purpose. The schema is defined by
[`docs/specs/02-data-model.md`](../docs/specs/02-data-model.md) and lands with
that spec's issue; this spec ships the runner only, so `migrate up` reports
that there is nothing to apply rather than failing.

The first migration must enable the trigram extension the product matching
depends on:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```
