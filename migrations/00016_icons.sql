-- +goose Up

-- The local icon library (docs/specs/42-local-icon-library.md): a vendored,
-- offline set of SVGs plus searchable names, served without any live call to
-- an external API. Tizian, reviewing the icon picker: "I do not want the
-- inventory to make an external call to iconify to search for icons... I
-- already have two dependencies, I do not want or need a third one."
--
-- Global, not per-storage — the same deliberate exception catalog_products
-- already is (docs/specs/02-data-model.md): an icon is not household data.
--
-- id carries no DEFAULT: every id in this schema is a UUIDv7 minted by the
-- application (migrations/README.md), so a missing one has to fail loudly
-- rather than fall back to a random v4.
CREATE TABLE icons (
    id         UUID PRIMARY KEY,
    name       VARCHAR(100) NOT NULL,
    svg_body   TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'vendored'
               CHECK (source IN ('vendored', 'uploaded')),
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- name is the value products.icon_name stores — no separate id indirection
-- (docs/specs/16-product-maintenance.md's icon_name contract is unchanged by
-- this table existing). Namespaced (noto:cheese-wedge, custom:whatever-a-
-- person-typed) so two sources can never collide on a bare word, enforced
-- here rather than left to the importer's discipline.
CREATE UNIQUE INDEX idx_icons_name ON icons(name);

-- Backs 40's alias search. pg_trgm is already enabled (00001_extensions.sql).
CREATE INDEX idx_icons_name_trgm ON icons USING gin (name gin_trgm_ops);

-- +goose Down

DROP INDEX IF EXISTS idx_icons_name_trgm;
DROP INDEX IF EXISTS idx_icons_name;
DROP TABLE IF EXISTS icons;
