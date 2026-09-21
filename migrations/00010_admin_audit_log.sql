-- +goose Up

-- Admin audit trail (docs/specs/18-operations-and-observability.md).
--
-- Admin actions are the ones that cross tenant boundaries — creating a user,
-- granting a membership, moderating the shared catalogue — so they are exactly
-- the actions that have to be reconstructable after the fact. Storage-scoped
-- user actions are deliberately absent: inventory_logs already records who
-- changed quantities, and this table is the operator surface only.
--
-- Append-only by convention rather than by grant: the application has no
-- UPDATE or DELETE path to it at all (internal/store/audit.go), which is the
-- enforcement a single-role deployment can actually offer.
CREATE TABLE admin_audit_log (
    id         UUID PRIMARY KEY,
    -- Nullable, and ON DELETE SET NULL: deleting a user must not delete the
    -- record of what they did, and an admin deleting another admin is one of
    -- the actions worth keeping. The page renders a null actor as "(deleted
    -- user)" rather than failing.
    actor_id   UUID REFERENCES users(id) ON DELETE SET NULL,
    -- e.g. 'user_created', 'storage_member_added', 'catalog_entry_deleted',
    -- 'settings_updated', 'user_password_reset'. Text rather than an enum
    -- so a new admin route adds a constant in Go and no migration.
    action     TEXT NOT NULL,
    -- The affected id or key, as text: a UUID for most actions, a settings
    -- key for settings_updated.
    target     TEXT,
    -- Action-specific facts — a created username, a granted storage id.
    -- Never a password, not even transiently: the Go writers take a typed
    -- struct per action and no password material is a field on any of them.
    details    JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The only query the page makes: newest first, paged. id is the tiebreaker
-- because ids are UUIDv7 and therefore sort the same way created_at does,
-- which keeps the ordering total when two rows share a timestamp — without
-- that, a cursor page could repeat or skip a row.
CREATE INDEX admin_audit_log_recent_idx
    ON admin_audit_log (created_at DESC, id DESC);

-- +goose Down

DROP INDEX IF EXISTS admin_audit_log_recent_idx;
DROP TABLE IF EXISTS admin_audit_log;
