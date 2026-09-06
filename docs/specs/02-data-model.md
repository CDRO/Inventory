# 02 — Data Model

Depends on: [`00-overview.md`](00-overview.md), [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md).

This supersedes the draft schema in `PROJECT_PLAN.md`, which lacked
multi-tenancy. Every table that holds household data is scoped to a
`storages` row, directly or transitively. The one deliberate exception is
`catalog_products` (below), which is global **and carries no storage
reference at all** so that it cannot reveal anything about other storages.

Migrations are goose SQL files under `/migrations`, invoked only through
Docker (`docker compose run --rm app /inventory migrate up`), per
`01-architecture-and-deployment.md`. The first migration must enable the
trigram extension used for matching:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```

## Entity overview

```
storages ─┬─< storage_members >─┬─ users ─< sessions
          │                     │ (users may belong to 0..N storages;
          │                     │  admin is a separate flag on users,
          │                     │  not a storage_members role)
          ├─< locations   (self-referencing tree)
          ├─< categories  (self-referencing tree)
          ├─< products ─── categories
          │      └─< inventory_batches >─ locations
          │              └─< inventory_logs (via product_id)
          └─< shopping_lists (see 07) ─< shopping_list_items

catalog_products   (global, anonymous — no storage reference)
settings           (global key/value app configuration)
jobs               (background job tracking, see 04)
```

## Tables

### `users`

```sql
CREATE TABLE users (
    id            SERIAL PRIMARY KEY,
    username      VARCHAR(100) NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,          -- argon2id
    display_name  VARCHAR(255) NOT NULL,
    is_admin      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`is_admin = TRUE` grants access to the admin view described in
`03-auth-and-multi-tenancy.md`. It is orthogonal to storage membership: an
admin does not automatically gain access to any storage's inventory data
by virtue of being admin — they must also be a `storage_members` row if
they want to use a storage, same as any other user. This column is read
server-side only; it is never sent to a client (see
`03-auth-and-multi-tenancy.md`).

### `sessions`

Server-side session store. The browser cookie holds only an opaque
session id — never a user id, role, or any other claim a client could
tamper with.

```sql
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,          -- cryptographically random, opaque
    user_id    INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);
```

Expired rows are deleted lazily on lookup and by a periodic sweep. Deleting
a row immediately revokes that session.

### `storages`

```sql
CREATE TABLE storages (
    id         SERIAL PRIMARY KEY,
    name       VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### `storage_members`

```sql
CREATE TABLE storage_members (
    storage_id INT NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id    INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    added_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (storage_id, user_id)
);
```

No `role` column. All members of a storage have identical read/write
rights over that storage's data — see `03-auth-and-multi-tenancy.md`.

### `locations`

Self-referencing tree, scoped per storage.

```sql
CREATE TABLE locations (
    id          SERIAL PRIMARY KEY,
    storage_id  INT NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    parent_id   INT REFERENCES locations(id) ON DELETE CASCADE,
    name        VARCHAR(255) NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT locations_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id)
);

CREATE INDEX idx_locations_storage_id ON locations(storage_id);
CREATE INDEX idx_locations_parent_id ON locations(parent_id);
```

Application code must additionally verify (not expressible as a simple
`CHECK`) that `parent_id`, when set, references a `locations` row with the
**same** `storage_id`, and that a re-parent does not create a cycle —
reject the write otherwise. Example resulting path: `Basement → Right
Shelf → Layer 2 → Front-Right`, each level a row whose `parent_id` points
at the row above.

### `categories`

Self-referencing tree, scoped per storage — same shape and same
application-level invariants as `locations` (same-storage parent, no
cycles, no self-parent). Example path: `Food → Dairy → Cheese`.

```sql
CREATE TABLE categories (
    id                      SERIAL PRIMARY KEY,
    storage_id              INT NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    parent_id               INT REFERENCES categories(id) ON DELETE CASCADE,
    name                    VARCHAR(255) NOT NULL,
    default_shelf_life_days INT,          -- NULL = inherit from ancestor / item_type
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT categories_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id)
);

CREATE INDEX idx_categories_storage_id ON categories(storage_id);
CREATE INDEX idx_categories_parent_id ON categories(parent_id);
```

`default_shelf_life_days` makes expiry rules data, not code: a value set
on `Food → Dairy` applies to every descendant category that does not set
its own. Resolution order is defined in
`08-expiration-and-classification.md`.

Deleting a category that is still referenced by products must be rejected
(`409`), same policy as locations with inventory.

### `products`

```sql
CREATE TABLE products (
    id          SERIAL PRIMARY KEY,
    storage_id  INT NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    name        VARCHAR(255) NOT NULL,
    category_id INT REFERENCES categories(id) ON DELETE RESTRICT,
    item_type   VARCHAR(20) NOT NULL DEFAULT 'long_shelf_life'
                CHECK (item_type IN ('perishable', 'long_shelf_life', 'non_perishable')),
    min_stock   INT NOT NULL DEFAULT 0,
    image_url   TEXT,
    icon_name   VARCHAR(100),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_products_storage_id ON products(storage_id);
CREATE INDEX idx_products_category_id ON products(category_id);
CREATE INDEX idx_products_name_trgm ON products USING gin (name gin_trgm_ops);
```

`category_id` must reference a category in the **same** storage —
application-enforced, like the tree invariants above. `item_type` drives
default-expiry behavior (`08-expiration-and-classification.md`) and
replaces the PRD draft's boolean `is_perishable`. The trigram index backs
the matching service in `07-shopping-list-reconciliation.md`.

### `catalog_products` (global, anonymous)

Purpose: when a user adds a product that some storage has already
described, skip the expensive identification path — no Gemini call, no
SerpAPI call — and offer the known data as a suggestion to copy.

```sql
CREATE TABLE catalog_products (
    id                      SERIAL PRIMARY KEY,
    normalized_name         VARCHAR(255) NOT NULL UNIQUE,  -- lowercased, trimmed, collapsed whitespace
    display_name            VARCHAR(255) NOT NULL,
    category_path           TEXT,          -- denormalized text, e.g. "Food > Dairy > Cheese"
    item_type               VARCHAR(20) NOT NULL
                            CHECK (item_type IN ('perishable', 'long_shelf_life', 'non_perishable')),
    image_url               TEXT,
    icon_name               VARCHAR(100),
    default_shelf_life_days INT,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_catalog_products_name_trgm
    ON catalog_products USING gin (normalized_name gin_trgm_ops);
```

**Privacy invariants — these are requirements, not suggestions:**

- No `storage_id`, no `user_id`, no quantities, no locations, no counts of
  any kind. A catalog row describes *what a product is*, never *who has
  it* or *how many storages know it*.
- Never expose row ids, timestamps, or ordering that could be used to
  infer how many storages exist or when another storage was active. API
  responses derived from this table return only display fields
  (`display_name`, `category_path`, `item_type`, `image_url`, `icon_name`,
  `default_shelf_life_days`).
- The category is stored as a denormalized **text path**, not a
  `categories` FK, precisely because categories are storage-scoped and an
  FK would create a cross-storage link.

**Population:** whenever a product is created or its descriptive fields are
edited in any storage, upsert the corresponding catalog row keyed on
`normalized_name` (last write wins; only fill `image_url`/`icon_name` if
non-empty, so a storage that skipped choosing an image does not blank out
a good existing entry).

**Consumption:** the matching service (`07-shopping-list-reconciliation.md`)
queries this table *before* calling any external API, and offers hits as
"known product" suggestions the user can accept (copying the fields into a
new storage-local `products` row, resolving `category_path` to local
`categories` rows, creating them if absent) or ignore.

### `inventory_batches`

A concrete quantity of a product at a specific location, with its own
expiration date. A product's total stock is the sum of its batches.

```sql
CREATE TABLE inventory_batches (
    id              SERIAL PRIMARY KEY,
    product_id      INT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    location_id     INT NOT NULL REFERENCES locations(id) ON DELETE RESTRICT,
    quantity        INT NOT NULL DEFAULT 1 CHECK (quantity >= 0),
    expiration_date DATE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_inventory_batches_product_id ON inventory_batches(product_id);
CREATE INDEX idx_inventory_batches_location_id ON inventory_batches(location_id);
```

A batch reaching `quantity = 0` is deleted in the same transaction that
decremented it — see `09-consumption-logging.md`. Application code must
verify `location_id` belongs to the same storage as the product.

### `inventory_logs`

Append-only audit trail of quantity changes. Never updated or deleted.

```sql
CREATE TABLE inventory_logs (
    id         SERIAL PRIMARY KEY,
    product_id INT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    batch_id   INT REFERENCES inventory_batches(id) ON DELETE SET NULL,
    change_qty INT NOT NULL,
    reason     VARCHAR(50) NOT NULL
               CHECK (reason IN ('purchase', 'consumption', 'audit', 'vision_ingestion')),
    created_by INT REFERENCES users(id) ON DELETE SET NULL,
    timestamp  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_inventory_logs_product_id ON inventory_logs(product_id);
CREATE INDEX idx_inventory_logs_timestamp ON inventory_logs(timestamp);
```

`change_qty` is signed: positive for additions (purchase, vision
ingestion), negative for consumption; `audit` may be either sign. Every
write to `inventory_batches.quantity` must be paired, in the same
transaction, with an `inventory_logs` row explaining it. This table is the
data source for `11-reporting-and-analytics.md`.

### `settings`

Global application configuration that must be changeable without a
redeploy — currently the effective Gemini model id (see AI model
resilience in `01-architecture-and-deployment.md`).

```sql
CREATE TABLE settings (
    key        VARCHAR(100) PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by INT REFERENCES users(id) ON DELETE SET NULL
);
```

A `settings` value always overrides the corresponding environment
variable. Only admins may write this table.

### `jobs`

Background job tracking for the asynchronous vision calls; shape and
lifecycle are defined in `04-backend-api-conventions.md`.

## Cross-references

- Multi-tenancy access control and the non-enumeration rules built on
  `storages`/`storage_members`/`sessions`: `03-auth-and-multi-tenancy.md`.
- `locations` tree populated/edited via `06-vision-shelf-ingestion.md`.
- `categories.default_shelf_life_days` resolution:
  `08-expiration-and-classification.md`.
- `catalog_products` lookup order and UI: `07-shopping-list-reconciliation.md`.
- `products.min_stock` and reorder logic: `10-reorder-and-shopping-export.md`.
- Shopping-list tables (`shopping_lists`, `shopping_list_items`): defined
  in `07-shopping-list-reconciliation.md`, since their shape is driven
  entirely by that feature's matching workflow.
