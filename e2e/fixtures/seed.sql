-- E2E fixture: a known admin, two ordinary users, one storage with a small
-- inventory (docs/specs/05-frontend-pwa-foundations.md).
--
-- Applied directly to the disposable E2E database after migrations, NOT
-- through the application layer, so fixed ids can be referenced by name from
-- the test files. Run via
--   docker compose -f docker-compose.e2e.yml exec -T db \
--     psql -U e2e -d e2e -f /fixtures/seed.sql
--
-- The admin row usually already exists by then. `migrate up` and `serve` both
-- bootstrap the initial admin from ADMIN_INITIAL_USERNAME/PASSWORD
-- (docs/specs/03-auth-and-multi-tenancy.md), which docker-compose.e2e.yml sets
-- to this same username and password — under a real UUIDv7 rather than the
-- placeholder below. The users insert therefore ignores *any* conflict, not
-- just an id conflict: `ON CONFLICT (id)` would let the username's unique
-- constraint abort this whole transaction. Nothing below references the
-- admin's id, so which row wins does not matter.
--
-- Every id below is a fixed, memorable placeholder — not a real uuid.NewV7()
-- output — because a fixture has to be the same value on every run to be
-- referenced by later statements and by name from a test file. This is
-- fixture-only: docs/specs/02-data-model.md requires application code to
-- generate real UUIDv7 values for every other insert in the system.
--
-- The password for every seeded user is "e2e-fixture-password". The hash
-- below is a real argon2id digest of that string, generated with this
-- repository's own internal/auth.HashPassword — not a placeholder string —
-- so it will verify successfully the moment POST /api/auth/login exists.

BEGIN;

INSERT INTO users (id, username, password_hash, display_name, is_admin) VALUES
  ('00000000-0000-7000-8000-000000000001', 'e2e-admin', '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'E2E Admin', true),
  ('00000000-0000-7000-8000-000000000002', 'e2e-alice',  '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'Alice',     false),
  ('00000000-0000-7000-8000-000000000003', 'e2e-bob',    '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'Bob',       false)
ON CONFLICT DO NOTHING;

-- Two storages, so the "member of storage A gets 404 for storage B"
-- required journey (docs/specs/05-frontend-pwa-foundations.md) has a second
-- storage to be denied access to. Bob is deliberately not a member of
-- either.
INSERT INTO storages (id, name) VALUES
  ('00000000-0000-7000-8000-000000000010', 'E2E Household'),
  ('00000000-0000-7000-8000-000000000011', 'E2E Other Household')
ON CONFLICT (id) DO NOTHING;

INSERT INTO storage_members (storage_id, user_id) VALUES
  ('00000000-0000-7000-8000-000000000010', '00000000-0000-7000-8000-000000000002'), -- Alice: household
  ('00000000-0000-7000-8000-000000000010', '00000000-0000-7000-8000-000000000003'), -- Bob: household
  ('00000000-0000-7000-8000-000000000011', '00000000-0000-7000-8000-000000000002')  -- Alice: other household too, for the switcher journey
ON CONFLICT DO NOTHING;

INSERT INTO locations (id, storage_id, name, description) VALUES
  ('00000000-0000-7000-8000-000000000020', '00000000-0000-7000-8000-000000000010', 'Pantry', 'Kitchen pantry shelf'),
  ('00000000-0000-7000-8000-000000000021', '00000000-0000-7000-8000-000000000010', 'Fridge', 'Kitchen fridge')
ON CONFLICT (id) DO NOTHING;

INSERT INTO categories (id, storage_id, name, default_shelf_life_days) VALUES
  ('00000000-0000-7000-8000-000000000030', '00000000-0000-7000-8000-000000000010', 'Canned Goods', 730)
ON CONFLICT (id) DO NOTHING;

-- Greek Yogurt is a second product, dedicated to the consumption suite
-- (docs/specs/09-consumption-logging.md) so its batches stay exactly as
-- seeded: ingestion.spec.js's "reviewed and confirmed" test also files a new
-- batch of Canned Tomatoes, and fullyParallel (playwright.config.js) gives no
-- guarantee that test runs before or after this one.
INSERT INTO products (id, storage_id, name, category_id, item_type, min_stock) VALUES
  ('00000000-0000-7000-8000-000000000040', '00000000-0000-7000-8000-000000000010', 'Canned Tomatoes', '00000000-0000-7000-8000-000000000030', 'long_shelf_life', 2),
  ('00000000-0000-7000-8000-000000000041', '00000000-0000-7000-8000-000000000010', 'Greek Yogurt', NULL, 'perishable', 0)
ON CONFLICT (id) DO NOTHING;

INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_source) VALUES
  ('00000000-0000-7000-8000-000000000050', '00000000-0000-7000-8000-000000000040', '00000000-0000-7000-8000-000000000020', 4, 'derived')
ON CONFLICT (id) DO NOTHING;

INSERT INTO inventory_logs (id, product_id, batch_id, change_qty, reason, created_by) VALUES
  ('00000000-0000-7000-8000-000000000060', '00000000-0000-7000-8000-000000000040', '00000000-0000-7000-8000-000000000050', 4, 'purchase', '00000000-0000-7000-8000-000000000002')
ON CONFLICT (id) DO NOTHING;

-- Two batches of Greek Yogurt at different locations, one nearer expiration
-- than the other, for the consumption review screen's batch picker to have
-- something to pick and split across.
INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_date, expiration_source) VALUES
  ('00000000-0000-7000-8000-000000000052', '00000000-0000-7000-8000-000000000041', '00000000-0000-7000-8000-000000000020', 4, NULL, 'derived'),
  ('00000000-0000-7000-8000-000000000053', '00000000-0000-7000-8000-000000000041', '00000000-0000-7000-8000-000000000021', 3, '2030-01-01', 'user')
ON CONFLICT (id) DO NOTHING;

INSERT INTO inventory_logs (id, product_id, batch_id, change_qty, reason, created_by) VALUES
  ('00000000-0000-7000-8000-000000000062', '00000000-0000-7000-8000-000000000041', '00000000-0000-7000-8000-000000000052', 4, 'purchase', '00000000-0000-7000-8000-000000000002'),
  ('00000000-0000-7000-8000-000000000063', '00000000-0000-7000-8000-000000000041', '00000000-0000-7000-8000-000000000053', 3, 'purchase', '00000000-0000-7000-8000-000000000002')
ON CONFLICT (id) DO NOTHING;

-- Two ingestion proposals ready for review (docs/specs/06-vision-shelf-ingestion.md),
-- in the shape internal/ingest writes. Seeded rather than produced by a real
-- upload because the stack has no vision model to call; the upload path itself
-- is covered by its 503 answer in e2e/specs/ingestion.spec.js. No photo: these
-- rows have no crop, which the review screen handles.
--
-- Job ...70 is reviewed and confirmed by the E2E suite: an existing product on
-- an existing shelf, a new product on a shelf the model proposed below Pantry,
-- and a false positive to reject. Job ...71 is discarded by it. Job ...72 is
-- only ever looked at, so the inbox test has a proposal no parallel test
-- consumes out from under it.
INSERT INTO jobs (id, storage_id, kind, status, payload, created_by) VALUES
  ('00000000-0000-7000-8000-000000000070', '00000000-0000-7000-8000-000000000010', 'shelf_ingestion', 'done',
   '{"mode":"shelf","location_hint_id":null,"rows":[
      {"row_id":"0","label":"Canned Tomatoes 400g","confidence":0.93,"quantity":2,"bounding_box":{"x":0.1,"y":0.2,"width":0.2,"height":0.3},
       "match":{"status":"exact_match","product":{"id":"00000000-0000-7000-8000-000000000040","name":"Canned Tomatoes"},"candidates":[],"catalog":null},
       "location":{"path":[{"name":"Pantry","location_id":"00000000-0000-7000-8000-000000000020","proposed":false}],"location_id":"00000000-0000-7000-8000-000000000020"}},
      {"row_id":"1","label":"Oat Milk 1L","confidence":0.81,"quantity":3,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[],"catalog":null},
       "location":{"path":[{"name":"Pantry","location_id":"00000000-0000-7000-8000-000000000020","proposed":false},{"name":"Top Shelf","location_id":null,"proposed":true}],"location_id":null}},
      {"row_id":"2","label":"Mystery Jar","confidence":0.22,"quantity":1,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[],"catalog":null},
       "location":{"path":[],"location_id":null}}
   ]}',
   '00000000-0000-7000-8000-000000000003'),
  ('00000000-0000-7000-8000-000000000071', '00000000-0000-7000-8000-000000000010', 'product_photo', 'done',
   '{"mode":"product","location_hint_id":null,"rows":[
      {"row_id":"0","label":"Rolled Oats","confidence":0.7,"quantity":1,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[],"catalog":null},
       "location":{"path":[],"location_id":null}}
   ]}',
   '00000000-0000-7000-8000-000000000003'),
  ('00000000-0000-7000-8000-000000000072', '00000000-0000-7000-8000-000000000010', 'shelf_ingestion', 'done',
   '{"mode":"shelf","location_hint_id":null,"rows":[
      {"row_id":"0","label":"Rice 1kg","confidence":0.9,"quantity":1,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[],"catalog":null},"location":{"path":[],"location_id":null}},
      {"row_id":"1","label":"Lentils 500g","confidence":0.9,"quantity":2,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[],"catalog":null},"location":{"path":[],"location_id":null}}
   ]}',
   '00000000-0000-7000-8000-000000000003')
ON CONFLICT (id) DO NOTHING;

-- One consumption proposal (docs/specs/09-consumption-logging.md), in the
-- shape internal/consume writes: no location placement, a stage-1-only match
-- against this storage's own products. Row 0 matches Greek Yogurt, which has
-- two batches (Pantry and Fridge, above) for the review screen's batch
-- picker; row 1 is unrecognized, for the manual-correction search path.
INSERT INTO jobs (id, storage_id, kind, status, payload, created_by) VALUES
  ('00000000-0000-7000-8000-000000000073', '00000000-0000-7000-8000-000000000010', 'consumption_photo', 'done',
   '{"rows":[
      {"row_id":"0","label":"Empty Yogurt Pot","confidence":0.88,"quantity":1,"bounding_box":null,
       "match":{"status":"exact_match","product":{"id":"00000000-0000-7000-8000-000000000041","name":"Greek Yogurt"},"candidates":[]}},
      {"row_id":"1","label":"Unlabeled Empty Jar","confidence":0.3,"quantity":1,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[]}}
   ]}',
   '00000000-0000-7000-8000-000000000003')
ON CONFLICT (id) DO NOTHING;

COMMIT;
