-- E2E fixture: a known admin, two ordinary users, and two storages with small
-- inventories of their own (docs/specs/05-frontend-pwa-foundations.md).
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

-- e2e-admin is deliberately a member of no storage. That is not an oversight
-- in the fixture, it is the state of a freshly deployed system: is_admin
-- grants the admin view and nothing else, so the bootstrap admin waits on a
-- storage_members row like everybody else
-- (docs/specs/03-auth-and-multi-tenancy.md). It is what
-- docs/specs/29-first-run-admin-guidance.md exists to make survivable, and
-- what e2e/specs/first-run-admin.spec.js's first journey asserts against.
--
-- e2e-nomad and e2e-admin-2 are the other two states that journey needs to be
-- told apart from it: someone with no storage who is *not* an admin, and an
-- admin who *does* have one.
INSERT INTO users (id, username, password_hash, display_name, is_admin) VALUES
  ('00000000-0000-7000-8000-000000000001', 'e2e-admin', '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'E2E Admin', true),
  ('00000000-0000-7000-8000-000000000002', 'e2e-alice',  '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'Alice',     false),
  ('00000000-0000-7000-8000-000000000003', 'e2e-bob',    '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'Bob',       false),
  ('00000000-0000-7000-8000-000000000004', 'e2e-nomad',   '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'Nomad',     false),
  ('00000000-0000-7000-8000-000000000005', 'e2e-admin-2', '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'Second Admin', true),
  ('00000000-0000-7000-8000-000000000006', 'e2e-casey',   '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'Casey',     false),
  -- Dana exists only for e2e/specs/barcode-recall.spec.js
  -- (docs/specs/20-barcode-recall.md). The capture-time offer's state —
  -- barcode_prompt_enabled and barcode_prompt_seen_at — is per *user*, and
  -- that suite turns the offer off and back on again. A user shared with any
  -- other suite would have those flips land under playwright.config.js's
  -- fullyParallel, so Dana belongs to nothing but the storage below.
  ('00000000-0000-7000-8000-000000000007', 'e2e-dana',    '$argon2id$v=19$m=19456,t=2,p=1$2wl6xn6XAM82zaixEVbcsA$W5rfKKaXBdXrHWnIN1cvo5O3JPmyC8+Q9Fjq/eAPe9U', 'Dana',      false)
ON CONFLICT DO NOTHING;

-- Two storages, so the "member of storage A gets 404 for storage B"
-- required journey (docs/specs/05-frontend-pwa-foundations.md) has a second
-- storage to be denied access to. Bob is deliberately a member of the first
-- one only: he is the caller who must be refused "E2E Other Household", and
-- being in exactly one storage also makes him the user for whom the switcher
-- must stay hidden entirely.
-- "E2E Admin Household" is a third storage with exactly one member, the
-- second admin, and (originally) no contents at all. It exists so that
-- docs/specs/29-first-run-admin-guidance.md's boundary — an admin who *does*
-- belong to a storage is never redirected — can be asserted without adding a
-- member to "E2E Household", which is the shared workhorse that the
-- create-and-list, ingestion, consumption and stocktake suites all write to
-- under fullyParallel. One membership also gives that admin the same
-- nothing-to-choose landing Bob gets, so the journey asserts the ordinary
-- flow rather than a picker.
-- "E2E Zero-Locations Household" is a fourth storage, with its own dedicated
-- member (Casey, who belongs to nothing else): e2e/specs/shopping-list.spec.js's
-- and e2e/specs/ingestion.spec.js's zero-locations location-quick-create tests
-- (docs/specs/26-location-quick-create.md) each need a storage that is
-- genuinely empty of locations *at the moment they load it*, and both create
-- one for real. Sharing "E2E Admin Household" between the two under
-- fullyParallel raced: whichever test's job ran first left a location behind
-- for the other to see. Casey exists so shopping-list.spec.js's test has its
-- own storage instead; "E2E Admin Household" stays dedicated to
-- ingestion.spec.js's version of the same test, and to
-- docs/specs/29-first-run-admin-guidance.md's boundary journey, which never
-- touches locations.
INSERT INTO storages (id, name) VALUES
  ('00000000-0000-7000-8000-000000000010', 'E2E Household'),
  ('00000000-0000-7000-8000-000000000011', 'E2E Other Household'),
  ('00000000-0000-7000-8000-000000000012', 'E2E Admin Household'),
  ('00000000-0000-7000-8000-000000000013', 'E2E Zero-Locations Household'),
  -- "E2E Barcode Household" is the fifth storage, Dana's alone, for
  -- docs/specs/20-barcode-recall.md. Barcodes are unique per storage by
  -- primary key, and the suite associates, deletes and re-associates the same
  -- code; doing that in a shared storage would collide with any other suite
  -- that later wanted one.
  ('00000000-0000-7000-8000-000000000014', 'E2E Barcode Household')
ON CONFLICT (id) DO NOTHING;

INSERT INTO storage_members (storage_id, user_id) VALUES
  ('00000000-0000-7000-8000-000000000010', '00000000-0000-7000-8000-000000000002'), -- Alice: household
  ('00000000-0000-7000-8000-000000000010', '00000000-0000-7000-8000-000000000003'), -- Bob: household
  ('00000000-0000-7000-8000-000000000011', '00000000-0000-7000-8000-000000000002'), -- Alice: other household too, for the switcher journey
  ('00000000-0000-7000-8000-000000000012', '00000000-0000-7000-8000-000000000005'), -- Second admin: their own, so an admin with a storage is not a special case of someone else's
  ('00000000-0000-7000-8000-000000000013', '00000000-0000-7000-8000-000000000006'), -- Casey: zero-locations household, and nothing else
  ('00000000-0000-7000-8000-000000000014', '00000000-0000-7000-8000-000000000007')  -- Dana: barcode household, and nothing else
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

-- "E2E Other Household" (...011), Alice's second storage. It held nothing at
-- all until now, which was enough for journey 8's non-disclosure check — Bob
-- is refused it whether or not it has contents — but not for two others:
--
--   * Journey 2 ("switches between them and sees each storage's own data")
--     can only show that the switch changed what is on screen if each storage
--     has data of its own. Garage exists so the tree differs from ...010's
--     Pantry/Fridge in both directions.
--   * Journey 6 (shopping-list reconciliation across all three match states)
--     runs here rather than in ...010, so that pasting a list cannot disturb
--     the products the ingestion and consumption suites assert against.
--
-- The three product names are chosen for what internal/matching does with
-- them, not for realism — see e2e/specs/shopping-list.spec.js for the
-- arithmetic. In short: "sourdough bread" hits Sourdough Bread alone and
-- clears ExactThreshold, while "milk" is equidistant from Oat Milk and Soy
-- Milk by construction (same word count, same word lengths, so pg_trgm scores
-- them identically), which puts the pair below AmbiguousMargin and forces
-- StatusAmbiguous no matter how the absolute similarity is tuned.
INSERT INTO locations (id, storage_id, name, description) VALUES
  ('00000000-0000-7000-8000-000000000022', '00000000-0000-7000-8000-000000000011', 'Garage', 'Other household garage shelf')
ON CONFLICT (id) DO NOTHING;

INSERT INTO products (id, storage_id, name, category_id, item_type, min_stock) VALUES
  ('00000000-0000-7000-8000-000000000042', '00000000-0000-7000-8000-000000000011', 'Sourdough Bread', NULL, 'perishable', 0),
  ('00000000-0000-7000-8000-000000000043', '00000000-0000-7000-8000-000000000011', 'Oat Milk', NULL, 'perishable', 0),
  ('00000000-0000-7000-8000-000000000044', '00000000-0000-7000-8000-000000000011', 'Soy Milk', NULL, 'perishable', 0)
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

-- One more shelf-ingestion job, dedicated to
-- e2e/specs/ingestion.spec.js's location-quick-create test
-- (docs/specs/26-location-quick-create.md). Two rows so that test can prove
-- one row's own edit survives the other row's use of the modal; neither row
-- proposes a location, so both start on the plain "Choose a location…"
-- placeholder rather than the "new" option ingestion.spec.js's other job
-- exercises. This job is only ever looked at, never confirmed, so it can be
-- revisited by that test however many times it is run.
INSERT INTO jobs (id, storage_id, kind, status, payload, created_by) VALUES
  ('00000000-0000-7000-8000-000000000074', '00000000-0000-7000-8000-000000000010', 'shelf_ingestion', 'done',
   '{"mode":"shelf","location_hint_id":null,"rows":[
      {"row_id":"0","label":"Chili Flakes","confidence":0.8,"quantity":1,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[],"catalog":null},
       "location":{"path":[],"location_id":null}},
      {"row_id":"1","label":"Cumin","confidence":0.8,"quantity":1,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[],"catalog":null},
       "location":{"path":[],"location_id":null}}
   ]}',
   '00000000-0000-7000-8000-000000000003')
ON CONFLICT (id) DO NOTHING;

-- One shelf-ingestion job in "E2E Admin Household" (...012), the one seeded
-- storage with zero locations, dedicated to
-- e2e/specs/ingestion.spec.js's zero-locations location-quick-create test
-- (docs/specs/26-location-quick-create.md's first acceptance criterion).
-- Never confirmed, so it can be revisited by that test however many times it
-- is run; created_by is the second admin, this storage's only member.
INSERT INTO jobs (id, storage_id, kind, status, payload, created_by) VALUES
  ('00000000-0000-7000-8000-000000000075', '00000000-0000-7000-8000-000000000012', 'shelf_ingestion', 'done',
   '{"mode":"shelf","location_hint_id":null,"rows":[
      {"row_id":"0","label":"Board Games Box","confidence":0.8,"quantity":1,"bounding_box":null,
       "match":{"status":"new_item","product":null,"candidates":[],"catalog":null},
       "location":{"path":[],"location_id":null}}
   ]}',
   '00000000-0000-7000-8000-000000000005')
ON CONFLICT (id) DO NOTHING;

-- "E2E Barcode Household" (...014), Dana's alone
-- (docs/specs/20-barcode-recall.md). One location to stock into and two
-- products: one the suite attaches a code to, one for the 409 that proves a
-- code names exactly one product per storage. Neither carries a catalog_id, so
-- associating a code here writes no global catalog_barcodes row and cannot
-- affect any other suite.
INSERT INTO locations (id, storage_id, name, description) VALUES
  ('00000000-0000-7000-8000-000000000026', '00000000-0000-7000-8000-000000000014', 'Larder', 'The only shelf in the barcode household')
ON CONFLICT (id) DO NOTHING;

-- Products for e2e/specs/barcode-recall.spec.js, one per test plus one
-- control.
--
-- They are kept separate because a barcode names exactly one product per
-- storage, and the offer only applies to a product that has none — so two
-- tests sharing a product would decide each other's outcome by running order.
-- That file runs serially (test.describe.configure), so the hazard is order,
-- not concurrency; it is still a hazard, and one product per test is what
-- removes it rather than documents it.
--
-- 'Hand-typed Beans' is the one deliberate exception: the turn-off test only
-- reads it through a call that is already disabled and writes nothing, and the
-- product-page test attaches and then removes its own code. 'Control Beans'
-- exists so the rejection test's control assertion does not become a third
-- writer of it.
INSERT INTO products (id, storage_id, name, category_id, item_type, min_stock) VALUES
  ('00000000-0000-7000-8000-000000000048', '00000000-0000-7000-8000-000000000014', 'Recall Beans',      NULL, 'long_shelf_life', 0),
  ('00000000-0000-7000-8000-000000000049', '00000000-0000-7000-8000-000000000014', 'Claimed Beans',     NULL, 'long_shelf_life', 0),
  ('00000000-0000-7000-8000-00000000004a', '00000000-0000-7000-8000-000000000014', 'Rival Beans',       NULL, 'long_shelf_life', 0),
  ('00000000-0000-7000-8000-00000000004b', '00000000-0000-7000-8000-000000000014', 'Offered Beans',     NULL, 'long_shelf_life', 0),
  ('00000000-0000-7000-8000-00000000004c', '00000000-0000-7000-8000-000000000014', 'Reoffered Beans',   NULL, 'long_shelf_life', 0),
  ('00000000-0000-7000-8000-00000000004d', '00000000-0000-7000-8000-000000000014', 'Unoffered Beans',   NULL, 'long_shelf_life', 0),
  ('00000000-0000-7000-8000-00000000004e', '00000000-0000-7000-8000-000000000014', 'Hand-typed Beans',  NULL, 'long_shelf_life', 0),
  ('00000000-0000-7000-8000-00000000004f', '00000000-0000-7000-8000-000000000014', 'Pre-coded Beans',   NULL, 'long_shelf_life', 0),
  ('00000000-0000-7000-8000-000000000050', '00000000-0000-7000-8000-000000000014', 'Control Beans',     NULL, 'long_shelf_life', 0)
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
