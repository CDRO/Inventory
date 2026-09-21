-- +goose Up

-- Expiry notifications (docs/specs/17-expiry-notifications.md).
--
-- One row per storage, not per user: the digest concerns shared food and the
-- natural target is a household channel everyone already subscribes to. The
-- primary key *is* the foreign key, so a storage cannot accumulate two
-- conflicting configurations, and ON DELETE CASCADE is what makes "deleting
-- the storage stops delivery immediately" a property of the schema rather
-- than of remembering to clean up.
--
-- enabled defaults to FALSE and the table starts empty: a household that
-- never opens the settings page is never notified, which is the whole
-- posture of the spec.
CREATE TABLE notification_settings (
    storage_id   UUID PRIMARY KEY REFERENCES storages(id) ON DELETE CASCADE,
    enabled      BOOLEAN NOT NULL DEFAULT FALSE,
    kind         TEXT NOT NULL CHECK (kind IN ('ntfy', 'gotify', 'webhook')),
    url          TEXT NOT NULL,
    -- Optional auth token, kind-specific. Write-only through the API: a read
    -- reports whether one is set, never the value.
    token        TEXT,
    send_hour    INT NOT NULL DEFAULT 8 CHECK (send_hour BETWEEN 0 AND 23),
    include_soon BOOLEAN NOT NULL DEFAULT FALSE,
    -- When the scheduler last claimed this row. It is the once-a-day guard,
    -- claimed before delivery is attempted so a crash mid-send costs one
    -- digest rather than repeating it on every restart — there is no retry
    -- queue by design, the next day's run is the retry.
    last_run_at  TIMESTAMPTZ,
    -- 'sent', 'empty', or a short failure summary. Never the transport
    -- error's own text: that carries the request URL, and a gotify URL
    -- carries the token in its query string.
    last_result  TEXT,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The scheduler's only query: every enabled storage whose configured hour is
-- the current one. Small table (one row per storage), but the index keeps the
-- hourly tick from scanning it and makes the predicate's intent explicit.
CREATE INDEX notification_settings_due_idx
    ON notification_settings (send_hour)
    WHERE enabled;

-- +goose Down

DROP INDEX IF EXISTS notification_settings_due_idx;
DROP TABLE IF EXISTS notification_settings;
