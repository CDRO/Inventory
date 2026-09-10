-- E2E fixture: a known admin, two ordinary users, one storage with a small
-- inventory (docs/specs/05-frontend-pwa-foundations.md).
--
-- Applied directly to the disposable E2E database after migrations, NOT
-- through the application layer: as of this fixture's authoring, there is no
-- API to create the first user, so this is the only way to seed one. Run via
--   docker compose -f docker-compose.e2e.yml exec -T db \
--     psql -U e2e -d e2e -f /fixtures/seed.sql
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
ON CONFLICT (id) DO NOTHING;

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
  ('00000000-0000-7000-8000-000000000020', '00000000-0000-7000-8000-000000000010', 'Pantry', 'Kitchen pantry shelf')
ON CONFLICT (id) DO NOTHING;

INSERT INTO categories (id, storage_id, name, default_shelf_life_days) VALUES
  ('00000000-0000-7000-8000-000000000030', '00000000-0000-7000-8000-000000000010', 'Canned Goods', 730)
ON CONFLICT (id) DO NOTHING;

INSERT INTO products (id, storage_id, name, category_id, item_type, min_stock) VALUES
  ('00000000-0000-7000-8000-000000000040', '00000000-0000-7000-8000-000000000010', 'Canned Tomatoes', '00000000-0000-7000-8000-000000000030', 'long_shelf_life', 2)
ON CONFLICT (id) DO NOTHING;

INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_source) VALUES
  ('00000000-0000-7000-8000-000000000050', '00000000-0000-7000-8000-000000000040', '00000000-0000-7000-8000-000000000020', 4, 'derived')
ON CONFLICT (id) DO NOTHING;

INSERT INTO inventory_logs (id, product_id, batch_id, change_qty, reason, created_by) VALUES
  ('00000000-0000-7000-8000-000000000060', '00000000-0000-7000-8000-000000000040', '00000000-0000-7000-8000-000000000050', 4, 'purchase', '00000000-0000-7000-8000-000000000002')
ON CONFLICT (id) DO NOTHING;

COMMIT;
