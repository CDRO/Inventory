-- +goose Up

-- Search aliases for the local icon library (docs/specs/40-icon-picker.md).
-- This table is unchanged from the picker's original draft — the earlier
-- design's network dependency was the live Iconify *search* call, which
-- 00016_icons.sql's vendored icons table already replaced; recording aliases
-- locally was never the part that needed to change.
--
-- Global, not per-storage — the same deliberate exception catalog_products
-- and icons already are (docs/specs/02-data-model.md): an alias describes a
-- locally-stored icon, not household data.
--
-- icon_name is NOT a foreign key to icons.name, deliberately: an alias
-- recorded against an icon a later re-import renames or drops degrades to
-- one fewer search hit, never a broken reference or a migration hazard.
--
-- id carries no DEFAULT, matching every other table in this schema: ids are
-- UUIDv7s minted by the application (migrations/README.md).
CREATE TABLE icon_aliases (
    id         UUID PRIMARY KEY,
    icon_name  VARCHAR(100) NOT NULL,
    alias      VARCHAR(100) NOT NULL,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Insert-and-ignore key (ON CONFLICT (icon_name, alias) DO NOTHING) — one
-- icon_name can have many aliases, and one alias can point at more than one
-- icon_name, so uniqueness is only on the pair.
CREATE UNIQUE INDEX idx_icon_aliases_icon_alias ON icon_aliases(icon_name, alias);

-- Backs fuzzy alias search. pg_trgm is already enabled (00001_extensions.sql).
CREATE INDEX idx_icon_aliases_alias_trgm ON icon_aliases USING gin (alias gin_trgm_ops);

-- +goose Down

DROP INDEX IF EXISTS idx_icon_aliases_alias_trgm;
DROP INDEX IF EXISTS idx_icon_aliases_icon_alias;
DROP TABLE IF EXISTS icon_aliases;
