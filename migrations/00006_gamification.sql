-- +goose Up

-- Gamification (docs/specs/50-gamification-overview.md,
-- docs/specs/51-gamification-scoring.md). Every id is a UUIDv7 supplied by
-- the application, with no DEFAULT, matching every other table
-- (docs/specs/02-data-model.md). Enum-like columns are TEXT with a CHECK
-- rather than VARCHAR(n): PostgreSQL stores both identically, and the CHECK
-- is the constraint that actually matters.

-- Server-written record of scoreable work that inventory_logs does not
-- capture (docs/specs/51-gamification-scoring.md). Deletable by an admin,
-- for a row recorded in error only — the one deletable ledger in the
-- system; inventory_logs stays append-only and untouched by this migration.
CREATE TABLE contribution_events (
    id         UUID PRIMARY KEY,
    storage_id UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL
               CHECK (kind IN ('ai_correction', 'metadata_filled', 'location_mapped',
                               'category_created', 'expiry_confirmed', 'ambiguity_resolved')),
    ref_id     UUID,          -- the product/location/batch the work applied to
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_contribution_events_storage_user
    ON contribution_events(storage_id, user_id, created_at);

-- Cached rollup. Always reconstructible from the two ledgers above via
-- `/inventory recompute-progress`; never authoritative on its own.
CREATE TABLE user_progress (
    storage_id       UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    xp               INT NOT NULL DEFAULT 0,
    level            INT NOT NULL DEFAULT 1,
    streak_weeks     INT NOT NULL DEFAULT 0,
    last_active_week DATE,          -- Monday of the last week with any scored activity
    recomputed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (storage_id, user_id)
);

CREATE TABLE achievements_unlocked (
    storage_id      UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    achievement_key TEXT NOT NULL,            -- see docs/specs/52-gamification-quests-and-ui.md
    unlocked_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (storage_id, user_id, achievement_key)
);

-- Per-user opt-out (principle 4, docs/specs/50-gamification-overview.md).
CREATE TABLE user_preferences (
    user_id              UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    gamification_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Weeks a user has marked as holiday; streaks pause rather than break (see
-- docs/specs/52-gamification-quests-and-ui.md).
CREATE TABLE holiday_weeks (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    week_start DATE NOT NULL,          -- Monday
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, week_start)
);

-- Per-storage toggle for the optional individual leaderboard (principle 3,
-- docs/specs/50-gamification-overview.md).
CREATE TABLE storage_gamification_settings (
    storage_id          UUID PRIMARY KEY REFERENCES storages(id) ON DELETE CASCADE,
    leaderboard_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    weekly_goal_items   INT NOT NULL DEFAULT 20,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS storage_gamification_settings;
DROP TABLE IF EXISTS holiday_weeks;
DROP TABLE IF EXISTS user_preferences;
DROP TABLE IF EXISTS achievements_unlocked;
DROP TABLE IF EXISTS user_progress;
DROP TABLE IF EXISTS contribution_events;
