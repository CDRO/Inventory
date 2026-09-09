-- +goose Up

-- Every id is a UUIDv7 supplied by the application (uuid.NewV7). The columns
-- carry no DEFAULT on purpose: PostgreSQL 16 has no native v7 generator, and a
-- missing id must fail loudly rather than fall back to a random v4 that breaks
-- index locality (docs/specs/02-data-model.md).

CREATE TABLE users (
    id            UUID PRIMARY KEY,
    username      VARCHAR(100) NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,          -- argon2id
    display_name  VARCHAR(255) NOT NULL,
    is_admin      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- sessions.id is the one deliberate exception to UUIDv7: a session id is a
-- bearer credential, and a v7 embeds a timestamp and is partially predictable.
-- 256 CSPRNG bits, base64url-encoded.
CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL DEFAULT 'browser'
                 CHECK (kind IN ('browser', 'device')),
    label        VARCHAR(100),            -- device name, e.g. "Pixel 9"; NULL for browser
    last_seen_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE storages (
    id         UUID PRIMARY KEY,
    name       VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- No role column: every member of a storage has identical read/write rights
-- over that storage's data (docs/specs/03-auth-and-multi-tenancy.md).
CREATE TABLE storage_members (
    storage_id UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    added_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (storage_id, user_id)
);

-- Self-referencing tree, scoped per storage. The CHECK stops a row being its
-- own parent; the same-storage parent rule and cycle prevention are not
-- expressible here and are enforced in internal/store.
CREATE TABLE locations (
    id          UUID PRIMARY KEY,
    storage_id  UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    parent_id   UUID REFERENCES locations(id) ON DELETE CASCADE,
    name        VARCHAR(255) NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT locations_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id)
);

CREATE INDEX idx_locations_storage_id ON locations(storage_id);
CREATE INDEX idx_locations_parent_id ON locations(parent_id);

-- Same shape and same application-level invariants as locations.
CREATE TABLE categories (
    id                      UUID PRIMARY KEY,
    storage_id              UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    parent_id               UUID REFERENCES categories(id) ON DELETE CASCADE,
    name                    VARCHAR(255) NOT NULL,
    default_shelf_life_days INT,          -- NULL = inherit from ancestor / item_type
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT categories_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id)
);

CREATE INDEX idx_categories_storage_id ON categories(storage_id);
CREATE INDEX idx_categories_parent_id ON categories(parent_id);

-- Global, anonymous and insert-only. It carries no storage_id, user_id,
-- quantity, location or count of any kind: a catalog row describes what a
-- product is, never who has it. Created before products because products
-- references it.
CREATE TABLE catalog_products (
    id                      UUID PRIMARY KEY,
    normalized_name         VARCHAR(255) NOT NULL UNIQUE,  -- lowercased, trimmed, collapsed whitespace
    display_name            VARCHAR(255) NOT NULL,
    base_id                 UUID REFERENCES catalog_products(id) ON DELETE SET NULL,
    category_path           TEXT,          -- denormalized text, e.g. "Food > Dairy > Cheese"
    item_type               TEXT NOT NULL
                            CHECK (item_type IN ('perishable', 'long_shelf_life', 'non_perishable')),
    image_url               TEXT,
    icon_name               VARCHAR(100),
    default_shelf_life_days INT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT catalog_not_own_base CHECK (base_id IS NULL OR base_id <> id)
);

CREATE INDEX idx_catalog_products_name_trgm
    ON catalog_products USING gin (normalized_name gin_trgm_ops);
CREATE INDEX idx_catalog_products_base_id ON catalog_products(base_id);

CREATE TABLE products (
    id                      UUID PRIMARY KEY,
    storage_id              UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    name                    VARCHAR(255) NOT NULL,
    category_id             UUID REFERENCES categories(id) ON DELETE RESTRICT,
    catalog_id              UUID REFERENCES catalog_products(id) ON DELETE SET NULL,
    item_type               TEXT NOT NULL DEFAULT 'long_shelf_life'
                            CHECK (item_type IN ('perishable', 'long_shelf_life', 'non_perishable')),
    default_shelf_life_days INT,          -- local override; NULL = resolve per spec 08
    min_stock               INT NOT NULL DEFAULT 0,
    image_url               TEXT,
    icon_name               VARCHAR(100),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_products_storage_id ON products(storage_id);
CREATE INDEX idx_products_category_id ON products(category_id);
CREATE INDEX idx_products_catalog_id ON products(catalog_id);
CREATE INDEX idx_products_name_trgm ON products USING gin (name gin_trgm_ops);

-- A batch is a quantity of one product at exactly ONE location; moving part of
-- one elsewhere is a split into two batches, never a batch with two homes.
-- location_id belonging to the product's storage is enforced in internal/store.
CREATE TABLE inventory_batches (
    id                UUID PRIMARY KEY,
    product_id        UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    location_id       UUID NOT NULL REFERENCES locations(id) ON DELETE RESTRICT,
    quantity          INT NOT NULL DEFAULT 1 CHECK (quantity >= 0),
    expiration_date   DATE,
    expiration_source TEXT NOT NULL DEFAULT 'derived'
                      CHECK (expiration_source IN ('derived', 'user')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_inventory_batches_product_id ON inventory_batches(product_id);
CREATE INDEX idx_inventory_batches_location_id ON inventory_batches(location_id);

-- Append-only audit trail. Never updated, never deleted: a wrong quantity is
-- corrected by a compensating row with reason = 'audit'.
CREATE TABLE inventory_logs (
    id         UUID PRIMARY KEY,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    batch_id   UUID REFERENCES inventory_batches(id) ON DELETE SET NULL,
    change_qty INT NOT NULL,
    reason     TEXT NOT NULL
               CHECK (reason IN ('purchase', 'consumption', 'audit',
                                 'vision_ingestion', 'move')),
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    timestamp  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_inventory_logs_product_id ON inventory_logs(product_id);
CREATE INDEX idx_inventory_logs_timestamp ON inventory_logs(timestamp);

-- A settings value always overrides the corresponding environment variable.
CREATE TABLE settings (
    key        VARCHAR(100) PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL
);

-- Listed under Tables in docs/specs/02-data-model.md; its shape and lifecycle
-- are specified in docs/specs/04-backend-api-conventions.md.
CREATE TABLE jobs (
    id           UUID PRIMARY KEY,
    storage_id   UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL
                 CHECK (kind IN ('shelf_ingestion', 'product_photo',
                                 'shopping_list_photo', 'consumption_photo')),
    status       TEXT NOT NULL
                 CHECK (status IN ('pending', 'done', 'failed', 'consumed')),
    payload      JSONB,
    error        TEXT,
    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_jobs_storage_status ON jobs(storage_id, status);

-- +goose Down
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS inventory_logs;
DROP TABLE IF EXISTS inventory_batches;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS catalog_products;
DROP TABLE IF EXISTS categories;
DROP TABLE IF EXISTS locations;
DROP TABLE IF EXISTS storage_members;
DROP TABLE IF EXISTS storages;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
