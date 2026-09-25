-- The assertion of the backup/restore round trip
-- (docs/specs/15-backup-restore-and-export.md, issue #133).
--
-- Run twice by the `restore-round-trip` job in .github/workflows/e2e.yml —
-- once against the seeded instance before it is destroyed, once against the
-- instance restored from the archive — and the two outputs are compared. The
-- restore is correct exactly when the two files are identical:
--
--   docker compose -f docker-compose.e2e.yml exec -T db \
--     psql -U e2e -d e2e -tAX -v ON_ERROR_STOP=1 < e2e/restore/snapshot.sql
--
-- Piped in on stdin rather than mounted: this file is an assertion, not a
-- fixture, and has no business in the /fixtures mount the db service gives
-- e2e/fixtures.
--
-- What it emits, one `key|value` line per fact, ordered by key:
--
--   count.<table>      a row count, for each of the ten tables below.
--   spot.<table>.<id>  the field values of one named row.
--
-- Both halves are needed and neither is sufficient. A `/healthz` 200 alone
-- says the app started, not that anything came back with it; one product
-- rendering on one page says nothing about a restore that silently dropped
-- every inventory_logs row, every admin_audit_log row, or the whole of a
-- second storage — the counts catch that. But counts alone pass a restore
-- that kept the row count and scrambled the contents, which is what the spot
-- checks are for. They deliberately span more than one storage, because
-- losing exactly one tenant is a plausible failure that a single-storage
-- assertion cannot see.
--
-- Two rules this file has to keep:
--
--   * No now(), CURRENT_DATE or age arithmetic anywhere. The seeded values
--     that were written relative (`CURRENT_DATE + 20`, `now() - interval '30
--     days'`) were materialized at seed time and travel in the dump as the
--     literals they became, so reading them back is stable — but a predicate
--     like `WHERE expiration_date < CURRENT_DATE` would turn a UTC-midnight
--     straddle between the two runs into a failure with nothing wrong.
--   * Every value is built from individually coalesced columns. Concatenating
--     one NULL column would make the whole expression NULL, and the row would
--     report itself missing when it is merely holding a null.
--
-- `sessions` is deliberately not counted *here*. The restore clears that
-- table on purpose (docs/specs/15-backup-restore-and-export.md: every session
-- from the backed-up instance is invalidated), so equal counts either side
-- would be exactly the wrong assertion to make. It is asserted instead by two
-- dedicated steps of the job, which require the opposite: seeded sessions
-- before the backup, none after the restore.
--
-- A spot check whose row is absent reports `<MISSING>` rather than vanishing
-- from the output, so a mistyped id here surfaces as a bad *before* snapshot
-- instead of quietly reducing the comparison to nothing. The workflow greps
-- the before snapshot for it.
--
-- That verdict is taken from the outer join — `CASE WHEN <alias>.<key> IS
-- NULL` — and deliberately not from the concatenated value. Every nullable
-- column is coalesced individually one line below, so an absent row would
-- otherwise render as a tidy line of `<null>`s that reads like data and
-- compares equal to the same absence after the restore. Verified by deleting
-- rows from a restored instance and watching which check noticed.

SELECT key, value FROM (

    -- Row counts. Which ten tables is this file's own choice, not a list
    -- issue #133 hands down — it asks only that "the seeded data is present
    -- and correct". These are the ten that carry the seeded fixture: the
    -- tenancy spine (users, storages, storage_members), the inventory it
    -- holds (locations, categories, products, inventory_batches), and the
    -- three append-only trails a lossy restore would strip in silence
    -- (inventory_logs, jobs, admin_audit_log).
    SELECT 'count.admin_audit_log'  AS key, count(*)::text AS value FROM admin_audit_log
    UNION ALL SELECT 'count.categories',        count(*)::text FROM categories
    UNION ALL SELECT 'count.inventory_batches', count(*)::text FROM inventory_batches
    UNION ALL SELECT 'count.inventory_logs',    count(*)::text FROM inventory_logs
    UNION ALL SELECT 'count.jobs',              count(*)::text FROM jobs
    UNION ALL SELECT 'count.locations',         count(*)::text FROM locations
    UNION ALL SELECT 'count.products',          count(*)::text FROM products
    UNION ALL SELECT 'count.storage_members',   count(*)::text FROM storage_members
    UNION ALL SELECT 'count.storages',          count(*)::text FROM storages
    UNION ALL SELECT 'count.users',             count(*)::text FROM users

    -- Two storages by name: "E2E Household" (...010), the shared workhorse,
    -- and "E2E Inventory" (...016), which has its own member, its own
    -- location tree and its own stock. Every spot check below names a row in
    -- one of those two, so a restore that brought one tenant back and not the
    -- other fails here rather than passing a count that happens to add up.
    UNION ALL
    SELECT 'spot.storages.' || e.id::text,
           CASE WHEN s.id IS NULL THEN '<MISSING>' ELSE 'name=' || s.name END
      FROM (VALUES ('00000000-0000-7000-8000-000000000010'::uuid),
                   ('00000000-0000-7000-8000-000000000016'::uuid)) AS e(id)
      LEFT JOIN storages s ON s.id = e.id

    -- start_page is per membership, not per user: e2e-start-multi's two rows
    -- differ on purpose, so a restore that dropped half the composite key
    -- cannot come back with both of them still right.
    UNION ALL
    SELECT 'spot.storage_members.' || e.storage_id::text || '.' || e.user_id::text,
           CASE WHEN m.storage_id IS NULL THEN '<MISSING>' ELSE 'start_page=' || m.start_page END
      FROM (VALUES ('00000000-0000-7000-8000-000000000010'::uuid, '00000000-0000-7000-8000-000000000002'::uuid),
                   ('00000000-0000-7000-8000-000000000016'::uuid, '00000000-0000-7000-8000-000000000009'::uuid),
                   ('00000000-0000-7000-8000-000000000018'::uuid, '00000000-0000-7000-8000-00000000000b'::uuid),
                   ('00000000-0000-7000-8000-000000000019'::uuid, '00000000-0000-7000-8000-00000000000b'::uuid))
             AS e(storage_id, user_id)
      LEFT JOIN storage_members m
             ON m.storage_id = e.storage_id AND m.user_id = e.user_id

    -- "Shelf A" (...0b1) hangs under "Cellar" (...0b0): a restore that loaded
    -- the rows but lost the self-referencing tree shows up as a null parent.
    UNION ALL
    SELECT 'spot.locations.' || e.id::text,
           CASE WHEN l.id IS NULL THEN '<MISSING>' ELSE
                'storage=' || coalesce(l.storage_id::text, '<null>')
             || ', parent=' || coalesce(l.parent_id::text, '<null>')
             || ', name=' || coalesce(l.name, '<null>')
             || ', description=' || coalesce(l.description, '<null>')
           END
      FROM (VALUES ('00000000-0000-7000-8000-000000000021'::uuid),
                   ('00000000-0000-7000-8000-0000000000b1'::uuid)) AS e(id)
      LEFT JOIN locations l ON l.id = e.id

    UNION ALL
    SELECT 'spot.categories.' || e.id::text,
           CASE WHEN c.id IS NULL THEN '<MISSING>' ELSE
                'storage=' || coalesce(c.storage_id::text, '<null>')
             || ', name=' || coalesce(c.name, '<null>')
             || ', default_shelf_life_days=' || coalesce(c.default_shelf_life_days::text, '<null>')
           END
      FROM (VALUES ('00000000-0000-7000-8000-000000000030'::uuid)) AS e(id)
      LEFT JOIN categories c ON c.id = e.id

    UNION ALL
    SELECT 'spot.products.' || e.id::text,
           CASE WHEN p.id IS NULL THEN '<MISSING>' ELSE
                'storage=' || coalesce(p.storage_id::text, '<null>')
             || ', name=' || coalesce(p.name, '<null>')
             || ', category=' || coalesce(p.category_id::text, '<null>')
             || ', item_type=' || coalesce(p.item_type, '<null>')
             || ', min_stock=' || coalesce(p.min_stock::text, '<null>')
             -- image_url is the column that ties a product row to a file in
             -- the archived uploads tree. Both seeded products leave it null
             -- today, so this reads `<null>` on both sides — it is here so the
             -- day a fixture does set one, the link is covered rather than
             -- quietly outside the check.
             || ', image_url=' || coalesce(p.image_url, '<null>')
           END
      FROM (VALUES ('00000000-0000-7000-8000-000000000040'::uuid),
                   ('00000000-0000-7000-8000-0000000000c3'::uuid)) AS e(id)
      LEFT JOIN products p ON p.id = e.id

    -- One batch per storage, each carrying a value a lossy restore would
    -- flatten: ...053 has a user-set expiration date, ...0d4 a derived one
    -- that was computed relative to seed time and is a stored literal by now.
    UNION ALL
    SELECT 'spot.inventory_batches.' || e.id::text,
           CASE WHEN b.id IS NULL THEN '<MISSING>' ELSE
                'product=' || coalesce(b.product_id::text, '<null>')
             || ', location=' || coalesce(b.location_id::text, '<null>')
             || ', quantity=' || coalesce(b.quantity::text, '<null>')
             || ', expiration_date=' || coalesce(b.expiration_date::text, '<null>')
             || ', expiration_source=' || coalesce(b.expiration_source, '<null>')
           END
      FROM (VALUES ('00000000-0000-7000-8000-000000000053'::uuid),
                   ('00000000-0000-7000-8000-0000000000d4'::uuid)) AS e(id)
      LEFT JOIN inventory_batches b ON b.id = e.id

    -- The ledger. Every inventory_batches write is paired with one of these
    -- in the same transaction (CLAUDE.md), so a restore that brought batches
    -- back without their logs has broken that pairing after the fact.
    UNION ALL
    SELECT 'spot.inventory_logs.' || e.id::text,
           CASE WHEN g.id IS NULL THEN '<MISSING>' ELSE
                'product=' || coalesce(g.product_id::text, '<null>')
             || ', batch=' || coalesce(g.batch_id::text, '<null>')
             || ', change_qty=' || coalesce(g.change_qty::text, '<null>')
             || ', reason=' || coalesce(g.reason, '<null>')
             || ', created_by=' || coalesce(g.created_by::text, '<null>')
           END
      FROM (VALUES ('00000000-0000-7000-8000-000000000063'::uuid),
                   ('00000000-0000-7000-8000-0000000000e4'::uuid)) AS e(id)
      LEFT JOIN inventory_logs g ON g.id = e.id

    -- payload is JSONB and the largest structured value in the fixture; its
    -- md5 is the cheap way to assert the whole document came back rather than
    -- merely came back non-null. jsonb normalizes key order, so the digest is
    -- stable for a given value.
    UNION ALL
    SELECT 'spot.jobs.' || e.id::text,
           CASE WHEN j.id IS NULL THEN '<MISSING>' ELSE
                'storage=' || coalesce(j.storage_id::text, '<null>')
             || ', kind=' || coalesce(j.kind, '<null>')
             || ', status=' || coalesce(j.status, '<null>')
             || ', error=' || coalesce(j.error, '<null>')
             || ', created_by=' || coalesce(j.created_by::text, '<null>')
             || ', payload_md5=' || coalesce(md5(j.payload::text), '<null>')
           END
      FROM (VALUES ('00000000-0000-7000-8000-000000000074'::uuid),
                   ('00000000-0000-7000-8000-0000000000a6'::uuid)) AS e(id)
      LEFT JOIN jobs j ON j.id = e.id

    -- Keyed by username, not by id: e2e-admin's row is the one `migrate up`
    -- bootstrapped under a real UUIDv7, so its id is not knowable from here.
    -- is_admin is included because it is the one column in this system that is
    -- always re-read from the database (CLAUDE.md) — a restore that lost it
    -- would silently demote or promote somebody.
    --
    -- password_hash is digested rather than printed. Not because the fixture
    -- hash is a secret — it is a real argon2id digest of the string
    -- e2e/fixtures/seed.sql names in plain sight — but because a restore that
    -- corrupted every hash in the database would otherwise pass this job
    -- completely green, and the symptom would be nobody being able to log in,
    -- discovered during an actual recovery. The md5 makes that a diff instead.
    UNION ALL
    SELECT 'spot.users.' || e.username,
           CASE WHEN u.username IS NULL THEN '<MISSING>' ELSE
                'display_name=' || coalesce(u.display_name, '<null>')
             || ', is_admin=' || coalesce(u.is_admin::text, '<null>')
             || ', password_hash_md5=' || coalesce(md5(u.password_hash), '<null>')
           END
      FROM (VALUES ('e2e-admin'), ('e2e-alice'), ('e2e-bob'), ('e2e-inventory')) AS e(username)
      LEFT JOIN users u ON u.username = e.username

    -- The admin audit trail, seeded by e2e/fixtures/restore-roundtrip.sql.
    -- created_at is rendered in UTC explicitly so the session's TimeZone
    -- setting cannot change the text; ...0103 carries the null actor the
    -- column exists for.
    UNION ALL
    SELECT 'spot.admin_audit_log.' || e.id::text,
           CASE WHEN a.id IS NULL THEN '<MISSING>' ELSE
                'actor=' || coalesce(a.actor_id::text, '<null>')
             || ', action=' || coalesce(a.action, '<null>')
             || ', target=' || coalesce(a.target, '<null>')
             || ', details=' || coalesce(a.details::text, '<null>')
             || ', created_at=' || coalesce(to_char(a.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'), '<null>')
           END
      FROM (VALUES ('00000000-0000-7000-8000-000000000101'::uuid),
                   ('00000000-0000-7000-8000-000000000102'::uuid),
                   ('00000000-0000-7000-8000-000000000103'::uuid)) AS e(id)
      LEFT JOIN admin_audit_log a ON a.id = e.id

) snapshot
ORDER BY key COLLATE "C";
