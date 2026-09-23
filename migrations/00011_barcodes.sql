-- +goose Up

-- Barcode recall (docs/specs/20-barcode-recall.md).
--
-- A barcode is never an identification here — docs/specs/00-overview.md still
-- rules out consulting an external UPC/EAN database, and nothing in this
-- migration or the code above it reaches one. What a barcode is, is a *recall
-- key*: once vision, the catalogue or a person has identified a product, the
-- code printed on the box becomes the cheapest possible way to find that same
-- product again.

-- One barcode maps to exactly one product per storage; one product may carry
-- many (multipack vs. single, a relabelled import).
--
-- storage_id is denormalized onto the row so that the per-storage uniqueness
-- is a database constraint rather than a rule the application remembers. It is
-- NOT a substitute for the same-storage check: products.storage_id has to be
-- verified to match, exactly like every other id crossing a storage boundary,
-- because a plain foreign key on product_id only proves the product exists.
CREATE TABLE product_barcodes (
    storage_id UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    barcode    VARCHAR(64) NOT NULL,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (storage_id, barcode)
);

-- The product's own barcode list (the edit screen in
-- docs/specs/16-product-maintenance.md) and the merge re-point both read by
-- product, which the (storage_id, barcode) primary key cannot serve.
CREATE INDEX idx_product_barcodes_product_id ON product_barcodes(product_id);

-- The global recall hint, on exactly the terms catalog_products has
-- (docs/specs/02-data-model.md): insert-only, no storage reference, no counts,
-- nothing in a response but display fields.
--
-- **Insert-only is a privacy and abuse property, not tidiness.** A globally
-- visible row any storage can rewrite is a covert channel between households,
-- and a barcode — printed, identical in every kitchen — is the strongest key
-- anyone could push a message through. The first mapping wins; a wrong first
-- mapping is removed by an admin (DELETE /api/admin/catalog-barcodes/{barcode}),
-- after which the next association may re-insert. There is deliberately no
-- UPDATE path anywhere in internal/store for this table.
--
-- No storage_id column exists here, and none may be added: a lookup hit is
-- allowed to reveal that a product with this code exists in the world, never
-- that another storage exists.
CREATE TABLE catalog_barcodes (
    barcode    VARCHAR(64) PRIMARY KEY,
    catalog_id UUID NOT NULL REFERENCES catalog_products(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Admin moderation deletes by barcode; the cascade from a removed catalog
-- product deletes by catalog_id, which the primary key cannot serve.
CREATE INDEX idx_catalog_barcodes_catalog_id ON catalog_barcodes(catalog_id);

-- The capture-time offer (docs/specs/20-barcode-recall.md, "Offering a barcode
-- at first capture").
--
-- Per-user, not per-storage: whether this person wants to be offered a scan
-- while the item is still in their hand is a preference about their own
-- workflow, the same category as the gamification opt-out. Like that one,
-- turning it off must never disable or degrade any inventory feature — only
-- the offer itself.
--
-- barcode_prompt_seen_at is what separates a user's first-ever occurrence of
-- the offer (playful, explanatory copy) from every later one (terse, same three
-- actions). NULL means never shown. It is written exactly once, by a single
-- conditional UPDATE (internal/store/barcodes.go), so "never shown twice" is a
-- WHERE clause rather than handler discipline.
ALTER TABLE users
    ADD COLUMN barcode_prompt_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN barcode_prompt_seen_at TIMESTAMPTZ;

-- +goose Down

ALTER TABLE users
    DROP COLUMN IF EXISTS barcode_prompt_seen_at,
    DROP COLUMN IF EXISTS barcode_prompt_enabled;

DROP INDEX IF EXISTS idx_catalog_barcodes_catalog_id;
DROP TABLE IF EXISTS catalog_barcodes;

DROP INDEX IF EXISTS idx_product_barcodes_product_id;
DROP TABLE IF EXISTS product_barcodes;
