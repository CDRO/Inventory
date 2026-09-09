-- +goose Up
-- The trigram extension backs the product matching in
-- docs/specs/07-shopping-list-reconciliation.md. It has to exist before any
-- gin_trgm_ops index is declared, which is why it is its own first migration.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- +goose Down
-- Deliberately not dropped: other schemas in the same database may depend on
-- the extension, and dropping it would take their indexes with it.
SELECT 1;
