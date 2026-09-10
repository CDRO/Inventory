-- +goose Up

-- Tables backing docs/specs/07-shopping-list-reconciliation.md: the shopping
-- list itself, its line items, and the suggestion-image cache.

-- One submitted list. `source` records how it arrived, because a photo list
-- reaches the same line items only after an OCR pass and that provenance is
-- worth keeping once the lines have been extracted.
CREATE TABLE shopping_lists (
    id          UUID PRIMARY KEY,
    storage_id  UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    source      TEXT NOT NULL CHECK (source IN ('text', 'photo')),
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_shopping_lists_storage_id ON shopping_lists(storage_id);

-- One line of the list, independently resolvable.
--
-- `status` is assigned once, by the matching service at ingestion. It is
-- deliberately not recomputed when the list is read: re-matching on every read
-- would let a catalog row added by another storage silently change what a user
-- is looking at mid-review.
--
-- matched_product_id is ON DELETE SET NULL rather than CASCADE: deleting a
-- product must not delete the shopping-list history that referenced it, and a
-- line item whose match has since been deleted is still a line the user wrote.
CREATE TABLE shopping_list_items (
    id                 UUID PRIMARY KEY,
    shopping_list_id   UUID NOT NULL REFERENCES shopping_lists(id) ON DELETE CASCADE,
    raw_text           VARCHAR(255) NOT NULL,
    status             TEXT NOT NULL
                       CHECK (status IN ('exact_match', 'new_item', 'ambiguous', 'resolved')),
    matched_product_id UUID REFERENCES products(id) ON DELETE SET NULL,
    resolved_quantity  INT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_shopping_list_items_list_id ON shopping_list_items(shopping_list_id);

-- The suggestion-image cache (docs/specs/07-shopping-list-reconciliation.md).
--
-- There is no storage_id here on purpose. The cache is keyed by the SHA-256 of
-- the source URL, so two storages searching for the same product share the
-- fetched bytes and the second one costs no provider call. Adding an owner
-- would both defeat that and create exactly the cross-storage signal the
-- catalog is careful not to carry (docs/specs/03-auth-and-multi-tenancy.md):
-- these rows describe a picture on the internet, never who looked for it.
--
-- `status = 'unusable'` is a metadata-only row with no file on disk: a
-- candidate that was too large, too many pixels, or simply would not decode.
-- Remembering that is the whole point — without negative caching, every run of
-- the same query re-downloads the same broken image forever.
CREATE TABLE cached_images (
    hash             TEXT PRIMARY KEY,     -- SHA-256 of source_url
    source_url       TEXT NOT NULL,
    content_type     TEXT NOT NULL,
    byte_size        BIGINT NOT NULL,
    width            INT,
    height           INT,
    status           TEXT NOT NULL DEFAULT 'ok'
                     CHECK (status IN ('ok', 'unusable')),
    fetched_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_accessed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index: eviction only ever walks usable rows, and 'unusable' rows
-- hold no file and so consume none of the byte budget.
CREATE INDEX idx_cached_images_lru ON cached_images(last_accessed_at)
    WHERE status = 'ok';

-- +goose Down
DROP TABLE IF EXISTS cached_images;
DROP TABLE IF EXISTS shopping_list_items;
DROP TABLE IF EXISTS shopping_lists;
