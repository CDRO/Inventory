# 02 — Data Model

Depends on: [`00-overview.md`](00-overview.md), [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md).

This supersedes the draft schema in `PROJECT_PLAN.md`, which lacked
multi-tenancy. Every table that holds household data is scoped to a
`storages` row, directly or transitively. The one deliberate exception is
`catalog_products` (below), which is global **and carries no storage
reference at all** so that it cannot reveal anything about other storages.

Migrations are goose SQL files under `/migrations`, invoked only through
Docker (`docker compose -f docker-compose.yml run --rm app migrate up`), per
`01-architecture-and-deployment.md` — the base file is pinned because `migrate`
lives in the compiled binary, which only the production image carries. The first migration must enable the
trigram extension used for matching:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```

## Primary keys: UUIDv7 everywhere

Every table uses `UUID` primary keys holding **UUIDv7** values, not
`SERIAL`. UUIDv7 is time-ordered, so it keeps the index locality that made
sequential ids attractive while removing their two problems here:

- **Sequential ids leak.** A user seeing `storage` ids 3 and 7 learns that
  other storages exist, and roughly how many — exactly what
  `03-auth-and-multi-tenancy.md` forbids. Opaque ids remove the inference
  entirely.
- Ids can be generated client-of-the-database side (in Go) before the
  insert, which simplifies multi-row transactional writes such as the
  ingestion confirm flow in `06-vision-shelf-ingestion.md`.

**Generation:** in Go, via `github.com/google/uuid` (`uuid.NewV7()`), and
passed explicitly on every insert. PostgreSQL 16 has no native UUIDv7
generator, so columns are declared with no default — a missing id is a
programming error that should fail loudly, not silently fall back to a
random v4 that breaks index locality.

```sql
id UUID PRIMARY KEY          -- always supplied by the application (uuid.NewV7)
```

**One deliberate exception: `sessions.id` stays a random opaque token, not
a UUIDv7.** A session id is a bearer credential; UUIDv7 embeds a
timestamp and is partially predictable, which is a bad property for a
secret. Use 256 bits from a CSPRNG, base64url-encoded.

All foreign keys are `UUID` accordingly; the DDL below shows this.

## Column type conventions

- **Enum-like columns use `TEXT` with a `CHECK` constraint**, never
  `VARCHAR(n)`. In PostgreSQL a `VARCHAR` length limit buys no storage or
  performance advantage over `TEXT` — values are length-prefixed either
  way — so when a `CHECK` already restricts the value to a fixed set, the
  length cap is redundant noise that additionally has to be widened
  (and, on older servers, rewritten) the first time a longer value is
  added. The `CHECK` is the real constraint; let it be the only one.
- `VARCHAR(n)` is kept only where the bound is a genuine domain rule about
  free text — names, usernames — not a guess at how long an identifier
  might get.

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

catalog_products     (global, anonymous — no storage reference; self-referencing variants)
settings             (global key/value app configuration)
jobs                 (background job tracking, see 04)
pairing_codes        (QR device pairing, see 12)
idempotency_records  (safe retries for offline clients, see 12)
tombstones           (deletions for delta sync, see 12)
```

## Tables

### `users`

```sql
CREATE TABLE users (
    id            UUID PRIMARY KEY,
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
    id           TEXT PRIMARY KEY,        -- 256-bit CSPRNG token, base64url; NOT a UUID
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
```

Expired rows are deleted lazily on lookup and by a periodic sweep. Deleting
a row immediately revokes that session.

`kind` and `label` exist so a paired native client
(`12-client-api-contract.md`) gets **its own session row** rather than sharing
the browser's. That is what makes "revoke my phone" possible without signing
the user out of their laptop. `kind = 'device'` rows carry a longer
`expires_at`; a phone that must re-pair weekly will not be used.

`last_seen_at` is updated at most once per hour per session — enough to show a
useful "last active" in the device list, without a database write on every
request.

### `storages`

```sql
CREATE TABLE storages (
    id         UUID PRIMARY KEY,
    name       VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### `storage_members`

```sql
CREATE TABLE storage_members (
    storage_id UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
    id                      UUID PRIMARY KEY,
    storage_id              UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    name                    VARCHAR(255) NOT NULL,
    category_id             UUID REFERENCES categories(id) ON DELETE RESTRICT,
    catalog_id              UUID REFERENCES catalog_products(id) ON DELETE SET NULL,
    item_type               TEXT NOT NULL DEFAULT 'long_shelf_life'
                            CHECK (item_type IN ('perishable', 'long_shelf_life', 'non_perishable')),
    default_shelf_life_days INT,          -- local override; NULL = resolve per 08
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
```

`category_id` must reference a category in the **same** storage —
application-enforced, like the tree invariants above. `item_type` drives
default-expiry behavior (`08-expiration-and-classification.md`) and
replaces the PRD draft's boolean `is_perishable`. The trigram index backs
the matching service in `07-shopping-list-reconciliation.md`.

`catalog_id` records which catalog entry this product was created from, if
any. It is **server-side only and never exposed in any API response** — it
exists so that an admin correcting a catalog entry's shelf life can
cascade that correction (`08-expiration-and-classification.md`). It points
at a global, anonymous row, so it reveals nothing about other storages.
Because `catalog_products` is defined after this table in the document,
the migration must create `catalog_products` first (or add this FK in a
follow-up migration).

`default_shelf_life_days` is this storage's own override for the product,
and wins over both the catalog value and the category chain; see the
resolution order in `08-expiration-and-classification.md`.

### `catalog_products` (global, anonymous, insert-only)

Purpose: when a user adds a product that some storage has already
described, skip the expensive identification path — no Gemini call, no
SerpAPI call — and offer the known data as a suggestion to copy.

```sql
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
```

**Privacy invariants — these are requirements, not suggestions:**

- No `storage_id`, no `user_id`, no quantities, no locations, no counts of
  any kind. A catalog row describes *what a product is*, never *who has
  it* or *how many storages know it*.
- Never expose row ids, timestamps, or ordering that could be used to
  infer how many storages exist or when another storage was active. API
  responses derived from this table return only display fields
  (`display_name`, `category_path`, `item_type`, `image_url`, `icon_name`,
  `default_shelf_life_days`, and variant siblings' display names).
- The category is stored as a denormalized **text path**, not a
  `categories` FK, precisely because categories are storage-scoped and an
  FK would create a cross-storage link.

**Population — insert-only. Rows are never updated after creation, with
one narrow admin exception (below).**

`INSERT ... ON CONFLICT (normalized_name) DO NOTHING`. There is no upsert,
no "last write wins", and no user-facing edit path — not from the app, not
from a product edit in any storage.

The reason is abuse, not tidiness: a globally-visible row that any storage
can rewrite is a covert messaging channel between households. Once anyone
notices that editing a product name changes what strangers see, it will be
used for that. Insert-only reduces the channel to a single one-shot write
by whoever first names a product, which is a far smaller surface and
cannot be used for a back-and-forth conversation.

Three consequences that must be implemented alongside it:

- **Editing a product in a storage never touches the catalog.** Local
  edits stay local. The catalog is a snapshot of how a product was first
  described, nothing more.
- **Catalog text is untrusted input.** It was written by a stranger.
  Render it with `textContent`, never as HTML (`05-frontend-pwa-foundations.md`),
  and never interpolate it into a prompt sent to Gemini.
- **`image_url` may only ever reference an image that came from a
  provider** — a SerpAPI result or an Iconify icon, normalized and stored
  locally (`07-shopping-list-reconciliation.md`). Never an arbitrary
  user-supplied URL: one pointing at someone else's server would let its
  owner change the picture other households see after the fact, and would
  expose viewers' IP addresses to them.
- **A user-uploaded photo is never published to the catalog.** When a
  product's image is a photo the user took — a custom upload, or a crop of
  their own shelf/product photo (`09-consumption-logging.md`) — the
  catalog row is inserted with `image_url` left `NULL`. That picture was
  taken inside someone's home; it may show their kitchen, their
  handwriting, their belongings. Names and categories are shareable
  metadata, private photographs are not, and no user should have to reason
  about which of their photos becomes globally visible. The product keeps
  the photo locally; the catalog simply has no image for that entry until
  some storage supplies a provider image for it.
- **Every uploaded image is stripped of EXIF and all other embedded
  metadata before it is stored**, catalog or not — see the mandatory
  stripping rule in `04-backend-api-conventions.md`. A phone photo carries
  GPS coordinates of the house, the device serial, and capture times; none
  of that belongs in an inventory system, and it must never survive as far
  as a file another person could open.
- **Admin moderation:** admins can delete a catalog row
  (`DELETE /api/admin/catalog/{id}`, `03-auth-and-multi-tenancy.md`), which
  is the only way a bad entry is removed. Deleting a catalog row never
  touches any storage's own `products`.

**The one permitted update: `default_shelf_life_days`, by an admin only.**

An admin may edit that single column
(`PATCH /api/admin/catalog/{id}`, `03-auth-and-multi-tenancy.md`), and the
change cascades to existing data per
`08-expiration-and-classification.md`. This does not reopen the abuse hole
the insert-only rule closes: the field is a **bounded integer written only
by an admin**, so it cannot carry a message to another household, unlike
the free-text and image fields, which stay permanently immutable. No other
column may be updated by anyone, ever.

**Variants (`base_id`) — handling "tomatoes" vs "cherry tomatoes".**

`base_id` is a self-reference to the more general form of the same
product: `cherry tomatoes → tomatoes`, `yellow tomatoes → tomatoes`. It
exists because plain string matching cannot resolve a shopping-list line
like `"thomatoes, c."` — trigram similarity gets from the typo to
`tomatoes`, but the `, c.` abbreviation for *cherry* is not recoverable
from the string. Matching the base and then offering its variants is what
actually answers that line.

- The graph is **built from real usage, not authored or AI-generated**:
  when a user is shown a catalog suggestion, rejects the exact name, and
  creates a differently-named product in the same interaction, the new
  catalog row is inserted with `base_id` pointing at the row they were
  shown. No curation step, no external call.
- Only **one level** is used. A variant's `base_id` must point at a row
  whose own `base_id` is `NULL`; if the user was shown a variant, link to
  that variant's base instead. Application-enforced — this keeps
  "siblings" a simple, cheap lookup and avoids chains that drift
  semantically.
- The matching service returns, alongside a base-name hit, its variant
  siblings as additional choices — see
  `07-shopping-list-reconciliation.md`.
- `base_id` is set at insert time only; like every other column here, it
  is never updated afterward.

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
```

A batch reaching `quantity = 0` is deleted in the same transaction that
decremented it — see `09-consumption-logging.md`. Application code must
verify `location_id` belongs to the same storage as the product.

**A batch lives at exactly one location.** Moving part of a batch
elsewhere is a *split* into two batches, not a batch with two homes; see
the split/move operations in `06-vision-shelf-ingestion.md`.

`expiration_source` records whether `expiration_date` was computed from
the shelf-life rules (`derived`) or typed by a person (`user`). It exists
so that an admin's shelf-life correction can cascade into existing
`derived` dates without ever overwriting a date a human deliberately set
(`08-expiration-and-classification.md`).

### `inventory_logs`

Append-only audit trail of quantity changes. Never updated or deleted.

```sql
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
```

`change_qty` is signed: positive for additions (purchase, vision
ingestion), negative for consumption; `audit` may be either sign. A
`move` (a batch split or relocation, `06-vision-shelf-ingestion.md`)
writes **two** rows that sum to zero — negative at the origin, positive at
the destination — so product totals are unaffected while per-location
figures stay correct. Every write to `inventory_batches.quantity` must be
paired, in the same transaction, with an `inventory_logs` row explaining
it. This table is the
data source for `11-reporting-and-analytics.md`, and — because it already
records who did what, when — the ledger the gamification specs
(`50-gamification-overview.md`) score from.

### `settings`

Global application configuration that must be changeable without a
redeploy — currently the effective Gemini model id (see AI model
resilience in `01-architecture-and-deployment.md`).

```sql
CREATE TABLE settings (
    key        VARCHAR(100) PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL
);
```

A `settings` value always overrides the corresponding environment
variable. Only admins may write this table.

### `jobs`

Background job tracking for the asynchronous vision calls; shape and
lifecycle are defined in `04-backend-api-conventions.md`.

### `pairing_codes`

Short-lived, single-use codes that let a native client obtain a session by
scanning a QR code instead of typing a password on a phone
(`12-client-api-contract.md`).

```sql
CREATE TABLE pairing_codes (
    code       TEXT PRIMARY KEY,          -- 256-bit CSPRNG, base64url
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,      -- created_at + 2 minutes
    used_at    TIMESTAMPTZ
);

CREATE INDEX idx_pairing_codes_expires_at ON pairing_codes(expires_at);
```

A code is redeemable exactly once: redemption sets `used_at`, and any later
attempt fails even inside the TTL. Expired and used rows are swept
periodically. Comparison is constant-time — this is the one unauthenticated
endpoint that mints a session, so it is the one worth guessing at.

### `idempotency_records`

Makes a client's retried write safe to repeat (`12-client-api-contract.md`).
A mobile client with an offline queue *will* re-send requests whose response it
never received; without this, the second attempt debits the pantry twice.

```sql
CREATE TABLE idempotency_records (
    key             TEXT NOT NULL,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    storage_id      UUID REFERENCES storages(id) ON DELETE CASCADE,
    request_hash    TEXT NOT NULL,        -- SHA-256 of method + path + body
    response_status INT NOT NULL,
    response_body   JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (key, user_id)
);

CREATE INDEX idx_idempotency_records_created_at ON idempotency_records(created_at);
```

Keyed by `(key, user_id)` so one client's key can never collide with another's.
`request_hash` is what distinguishes a genuine replay from a client bug: the
same key with a different body is `422`, not a silent replay. Rows are retained
7 days, then swept.

### `tombstones`

Records deletions of client-cacheable entities so a delta sync can tell a
client that a row is *gone* (`12-client-api-contract.md`). A delta built only
from `updated_at` can never express a deletion, so without this a deleted
product lives in a client's cache forever.

```sql
CREATE TABLE tombstones (
    id          UUID PRIMARY KEY,
    storage_id  UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    entity_type TEXT NOT NULL
                CHECK (entity_type IN ('product', 'category', 'location',
                                       'shopping_list', 'shopping_list_item')),
    entity_id   UUID NOT NULL,
    deleted_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_tombstones_storage_deleted ON tombstones(storage_id, deleted_at);
```

Written in the same transaction as the delete. Retained **30 days**; a client
whose `updated_since` predates the oldest surviving tombstone cannot be brought
up to date safely and is told to resync (`12-client-api-contract.md`).

### `updated_at` on cacheable entities

`products`, `categories`, `locations`, `shopping_lists` and
`shopping_list_items` each carry:

```sql
updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
```

set on every write, so a client can ask for "everything changed since X". Set
it in application code on the same statement as the change rather than with a
trigger, so the mechanism is visible where the write happens. Rows that only
these five tables need — not `inventory_logs`, which is append-only, nor
`inventory_batches`, whose changes are already visible through their product.

## Cross-references

- Multi-tenancy access control and the non-enumeration rules built on
  `storages`/`storage_members`/`sessions`: `03-auth-and-multi-tenancy.md`.
- `locations` tree populated/edited via `06-vision-shelf-ingestion.md`.
- `categories.default_shelf_life_days` resolution:
  `08-expiration-and-classification.md`.
- `catalog_products` lookup order, variant suggestions, and UI:
  `07-shopping-list-reconciliation.md`.
- `products.min_stock` and reorder logic: `10-reorder-and-shopping-export.md`.
- Shopping-list tables (`shopping_lists`, `shopping_list_items`) and the
  suggestion-image cache (`cached_images`): defined in
  `07-shopping-list-reconciliation.md`, since their shape is driven
  entirely by that feature's matching and image workflow.
- Gamification tables (`user_progress`, `quests`, `achievements`): defined
  in `51-gamification-scoring.md`. They are additive and optional —
  nothing in specs `00`–`11` may depend on them.
