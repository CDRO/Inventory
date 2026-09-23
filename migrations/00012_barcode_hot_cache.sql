-- +goose Up

-- Barcode hot cache (docs/specs/24-barcode-hot-cache.md).
--
-- One counter, on the one table already designed to be visible instance-wide
-- with no storage reference (docs/specs/20-barcode-recall.md): counting scans
-- against catalog_barcodes adds no new cross-storage visibility, because a
-- barcode with only a private product_barcodes association never gets a
-- catalog_barcodes row and so never participates.
--
-- No response anywhere may ever serialize this column — GET /api/barcodes/hot
-- conveys rank through array order only. A plain LIMIT 500 ordered scan needs
-- no supporting index at household/instance scale; revisit only if a real
-- deployment shows otherwise.
ALTER TABLE catalog_barcodes
    ADD COLUMN scan_count BIGINT NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE catalog_barcodes
    DROP COLUMN IF EXISTS scan_count;
