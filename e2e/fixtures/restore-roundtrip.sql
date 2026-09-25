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

-- Two live sessions, for the half of spec 15's restore criterion that the
-- before/after snapshot cannot express: "all prior sessions are invalid".
--
-- Everything else about a restore is a claim that a row came back, and the
-- snapshot asserts those by requiring the two sides to be identical. This one
-- is the opposite claim — that these rows are gone — so it cannot live in the
-- same comparison, and it gets two steps of the job to itself instead: these
-- rows must exist before the backup, and the table must be empty after the
-- restore. Without them the "after" check would be 0 = 0 against a table that
-- was never populated, which is the vacuous assertion this whole package
-- exists to avoid.
--
-- It is worth having because the session wipe is the one thing a restore does
-- that is not simply replaying the dump: scripts/backup runs a DELETE of its
-- own after loading db.sql. A session id is an opaque random string looked up
-- in this table rather than a signed token
-- (docs/specs/03-auth-and-multi-tenancy.md), so a restore that skipped that
-- DELETE would hand back working cookies from before the disaster, including
-- any stolen along the way. Nothing else in the repository executes that path.
--
-- Both kinds, because a paired device is the same table and the same kind of
-- credential, and the spec retires both. expires_at is far future so the
-- app's own expired-session sweep (cmd/inventory/main.go) cannot remove them
-- before the backup is taken and quietly make the "before" check vacuous
-- again. No user of these is ever logged in — the job asserts on the rows,
-- not through the browser.
INSERT INTO sessions (id, user_id, kind, label, expires_at) VALUES
  ('e2e-restore-roundtrip-browser-session', '00000000-0000-7000-8000-000000000002', 'browser', NULL,      '2099-01-01T00:00:00Z'),
  ('e2e-restore-roundtrip-device-session',  '00000000-0000-7000-8000-000000000003', 'device',  'E2E Pixel','2099-01-01T00:00:00Z')
ON CONFLICT (id) DO NOTHING;

COMMIT;
