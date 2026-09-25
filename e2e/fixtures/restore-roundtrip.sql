-- E2E fixture: the one thing the restore round trip counts that
-- e2e/fixtures/seed.sql does not seed
-- (docs/specs/15-backup-restore-and-export.md, issue #133).
--
-- Loaded only by the `restore-round-trip` job in .github/workflows/e2e.yml,
-- after seed.sql, and never by the browser suite — so no Playwright journey
-- can read these rows or be disturbed by them:
--   docker compose -f docker-compose.e2e.yml exec -T db \
--     psql -U e2e -d e2e -f /fixtures/restore-roundtrip.sql
--
-- Why it exists. The round trip asserts that every table's row count survives
-- a backup, a `down -v` and a restore, and admin_audit_log is the one table on
-- that list seed.sql leaves empty. A count assertion over an empty table is
-- satisfied by a restore that lost the table's contents entirely — 0 = 0 — so
-- without these rows the admin audit trail would be a thing the round trip
-- claims to check and does not. Every other table on the list
-- (users, storages, storage_members, locations, categories, products,
-- inventory_batches, inventory_logs, jobs) already has seeded rows.
--
-- Written as plain inserts rather than driven through the admin API, because
-- what is under test is whether rows come back from an archive, not how they
-- were made. The application has no UPDATE or DELETE path to this table at all
-- (migrations/00010_admin_audit_log.sql), so nothing can disturb these rows
-- between the two snapshots either.
--
-- Ids continue seed.sql's fixed-placeholder convention in a range that file
-- has not reached: it stops at ...0000f9, plus the deliberately-unknown
-- ...0000ff and ...00ffff its negative tests use, so ...000101 upward is free.
-- ON CONFLICT (id) DO NOTHING like every other fixture row, because a
-- colliding id would be dropped in silence and the snapshot would then be
-- taken of somebody else's row.

BEGIN;

-- Fixed timestamps rather than now(): the snapshot compares stored values
-- either side of a restore, and a literal is the least surprising thing to
-- read in a diff. (A now() here would work too — it is materialized at seed
-- time and travels in the dump — but nothing about this fixture needs to be
-- relative to today.)
--
-- The actor is e2e-admin-2 (...0005) and deliberately not e2e-admin (...0001):
-- that id does not exist. seed.sql's admin row loses its ON CONFLICT race to
-- the real bootstrap admin, which `migrate up` created under a genuine UUIDv7
-- from ADMIN_INITIAL_USERNAME, so an actor_id of ...0001 would fail this
-- table's foreign key.
--
-- The third row's null actor is the "(deleted user)" case the column is
-- nullable for, and it is here on purpose: a restore that turned nulls into
-- something else, or dropped the rows that have one, has to fail the snapshot
-- rather than slip through as a count that still adds up.
INSERT INTO admin_audit_log (id, actor_id, action, target, details, created_at) VALUES
  ('00000000-0000-7000-8000-000000000101', '00000000-0000-7000-8000-000000000005',
   'user_created', '00000000-0000-7000-8000-000000000006',
   '{"username":"e2e-casey"}', '2025-06-01T11:00:00Z'),
  ('00000000-0000-7000-8000-000000000102', '00000000-0000-7000-8000-000000000005',
   'storage_member_added', '00000000-0000-7000-8000-000000000013',
   '{"user_id":"00000000-0000-7000-8000-000000000006","storage":"E2E Zero-Locations Household"}',
   '2025-06-01T11:05:00Z'),
  ('00000000-0000-7000-8000-000000000103', NULL,
   'settings_updated', 'catalog_moderation',
   '{"value":"on"}', '2025-06-01T11:10:00Z')
ON CONFLICT (id) DO NOTHING;

COMMIT;
