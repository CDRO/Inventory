-- +goose Up

-- Stocktake (docs/specs/13-stocktake-and-audit.md).
--
-- One nullable column on an existing table. NULL means "never audited", which
-- is the honest starting state for every location that already exists — a
-- default of now() would claim every shelf in the system had just been walked,
-- and the whole point of the column is that staleness is visible.
--
-- Written only by completing a stocktake, never by a rename or a re-parent:
-- the timestamp answers "when did a person last stand in front of this shelf
-- and compare it against the database", and an edit to the tree is not that.
ALTER TABLE locations ADD COLUMN last_audited_at TIMESTAMPTZ;

-- +goose Down

ALTER TABLE locations DROP COLUMN last_audited_at;
