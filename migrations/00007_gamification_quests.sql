-- +goose Up

-- Weekly quests (docs/specs/52-gamification-quests-and-ui.md). One row per
-- (storage, week, generator): the UNIQUE constraint is what makes weekly
-- generation idempotent — re-running it for a week that already has rows
-- simply conflicts on the generators that already fired rather than
-- duplicating them.
CREATE TABLE quests (
    id           UUID PRIMARY KEY,
    storage_id   UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    week_start   DATE NOT NULL,           -- Monday
    generator    TEXT NOT NULL,
    params       JSONB NOT NULL,          -- target ids/counts resolved at generation time
    target_count INT NOT NULL,
    xp_reward    INT NOT NULL,
    completed_at TIMESTAMPTZ,
    UNIQUE (storage_id, week_start, generator)
);

CREATE INDEX idx_quests_storage_week ON quests(storage_id, week_start);

-- Per-quest, per-contributor XP attribution (docs/specs/52-gamification-quests-and-ui.md):
-- every member who logged at least one qualifying action gets the full
-- reward once, at completion — not split, not finisher-only. A separate
-- table rather than an array column because "has this (quest, user) pair
-- already been paid" needs to be an indexed existence check, not a linear
-- scan of a JSON blob.
CREATE TABLE quest_contributors (
    quest_id UUID NOT NULL REFERENCES quests(id) ON DELETE CASCADE,
    user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (quest_id, user_id)
);

-- clean_since (docs/specs/52-gamification-quests-and-ui.md): the date from
-- which no weekly generation has had any generator fire. NULL means the
-- storage is not currently on a clean streak. well_stocked_since tracks the
-- same shape of fact for the well_stocked achievement: no product below
-- min_stock. Both live on the existing per-storage settings row rather than
-- a new table, since each storage has exactly one of each.
ALTER TABLE storage_gamification_settings
    ADD COLUMN clean_since        DATE,
    ADD COLUMN well_stocked_since DATE;

-- +goose Down
ALTER TABLE storage_gamification_settings
    DROP COLUMN IF EXISTS well_stocked_since,
    DROP COLUMN IF EXISTS clean_since;
DROP TABLE IF EXISTS quest_contributors;
DROP TABLE IF EXISTS quests;
