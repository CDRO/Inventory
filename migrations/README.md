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
| `00004_shopping_and_image_cache.sql` | `shopping_lists`, `shopping_list_items`, `cached_images` — the reconciliation flow and its suggestion-image cache in [`docs/specs/07-shopping-list-reconciliation.md`](../docs/specs/07-shopping-list-reconciliation.md) |
| `00005_ingestion.sql` | `jobs.image_filename` and `jobs.location_hint_id` — the photo and shelf hint behind a review job in [`docs/specs/06-vision-shelf-ingestion.md`](../docs/specs/06-vision-shelf-ingestion.md) |
| `00006_gamification.sql` | `contribution_events`, `user_progress`, `achievements_unlocked`, `user_preferences`, `holiday_weeks`, `storage_gamification_settings` — the scoring ledger and caches of [`docs/specs/51-gamification-scoring.md`](../docs/specs/51-gamification-scoring.md) |
| `00007_gamification_quests.sql` | `quests`, `quest_contributors`, plus `clean_since`/`well_stocked_since` on `storage_gamification_settings` — weekly quests and the clean-storage/well-stocked milestones of [`docs/specs/52-gamification-quests-and-ui.md`](../docs/specs/52-gamification-quests-and-ui.md) |
| `00008_stocktake.sql` | `locations.last_audited_at` — the "when was this shelf last walked" timestamp of [`docs/specs/13-stocktake-and-audit.md`](../docs/specs/13-stocktake-and-audit.md) |
| `00009_notifications.sql` | `notification_settings` — the per-storage, opt-in expiry digest configuration of [`docs/specs/17-expiry-notifications.md`](../docs/specs/17-expiry-notifications.md) |
| `00010_admin_audit_log.sql` | `admin_audit_log` — the append-only record of admin actions in [`docs/specs/18-operations-and-observability.md`](../docs/specs/18-operations-and-observability.md) |
| `00011_barcodes.sql` | `product_barcodes`, `catalog_barcodes`, and the two `users` columns behind the capture-time offer in [`docs/specs/20-barcode-recall.md`](../docs/specs/20-barcode-recall.md) |
| `00012_barcode_hot_cache.sql` | `catalog_barcodes.scan_count` — the instance-wide scan popularity counter behind the client's hot-cache preview in [`docs/specs/24-barcode-hot-cache.md`](../docs/specs/24-barcode-hot-cache.md) |
| `00013_storage_member_start_page.sql` | `storage_members.start_page` — the per-person, per-storage start page of [`docs/specs/34-navigation-and-start-page.md`](../docs/specs/34-navigation-and-start-page.md). Grants nothing: `storage_members` still carries no role and no rights |

Every file carries both `-- +goose Up` and `-- +goose Down`. **Only one of those
down blocks is covered by a test**, and it is the newest:
`TestRunDownRollsBackTheRepositorysOwnMigrations`
(`internal/migrate/migrate_test.go`) rolls the shipped migrations back from the
top down to `00013`, checks against `information_schema` that the column that
migration adds is gone, then migrates up again. Nothing exercises the down block
of `00001`–`00012`, and no script, workflow or deployment step in this
repository runs `migrate down` at all.

So if you edit an older down block, no test will contradict you. That is a
deliberate consequence of the policy in
[`docs/specs/18-operations-and-observability.md`](../docs/specs/18-operations-and-observability.md),
not an oversight: production never rolls back with `migrate down` — restoring
the pre-upgrade backup is the rollback — precisely because down-migrations
against real data are "tested never and trusted always". Treat every down block
as untested unless you extend that test to reach it.

`00001`'s down step deliberately does **not** drop the extension. Other schemas
in the same database may depend on `pg_trgm`, and dropping it would take their
indexes with it.

## Adding a migration

Number it in sequence and keep it forward-only once merged — a migration that
has run on the NAS must never be edited, only superseded. The application
supplies every `id` as a UUIDv7 (`uuid.NewV7`), so new tables declare
`id UUID PRIMARY KEY` with **no** default: a missing id has to fail loudly
rather than fall back to a random v4 that breaks index locality.
