-- +goose Up

-- What a batch is physically held in — a 24-pack box, a bag, a cooler someone
-- packed for a trip — independent of where that thing currently sits
-- (docs/specs/39-batch-containers.md, docs/specs/02-data-model.md).
--
-- Deliberately not a second location hierarchy: a container holds exactly the
-- one batch that references it, and its remaining count *is* that batch's
-- quantity. There is no count column here, because a second number is a number
-- that can drift from the truth.
--
-- id carries no DEFAULT: every id in this schema is a UUIDv7 minted by the
-- application (migrations/README.md), so a missing one has to fail loudly
-- rather than fall back to a random v4.
CREATE TABLE containers (
    id             UUID PRIMARY KEY,
    storage_id     UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    label          VARCHAR(255) NOT NULL,
    container_type TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set by POST /api/storages/{id}/containers/{id}/destroy, and never
    -- cleared: "I threw the box away" is a statement about the physical
    -- object, and the column is its own audit trail. A destroyed row is kept
    -- forever so a batch's history can still name the container it was in.
    destroyed_at   TIMESTAMPTZ
);

CREATE INDEX idx_containers_storage_id ON containers(storage_id);

-- ON DELETE SET NULL, not CASCADE: a container is never a required field, and
-- nothing about removing one may cascade into batch data or fail a batch write.
--
-- No storage_id cross-check lives in this FK, and none can: a foreign key
-- proves the referenced row exists, never which storage owns it. That check is
-- application code's job here exactly as it is for location_id (CLAUDE.md,
-- docs/specs/03-auth-and-multi-tenancy.md).
ALTER TABLE inventory_batches
    ADD COLUMN container_id UUID REFERENCES containers(id) ON DELETE SET NULL;

-- Partial, because the only query that reads this column the other way round —
-- the destroy sweep clearing every batch that referenced a container — asks
-- about a specific container id, and the overwhelming majority of batches have
-- no container at all.
CREATE INDEX idx_inventory_batches_container_id
    ON inventory_batches(container_id) WHERE container_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_inventory_batches_container_id;

ALTER TABLE inventory_batches
    DROP COLUMN IF EXISTS container_id;

DROP INDEX IF EXISTS idx_containers_storage_id;

DROP TABLE IF EXISTS containers;
