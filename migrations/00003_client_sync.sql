-- +goose Up

-- Tables backing the third-party client contract in
-- docs/specs/12-client-api-contract.md.

-- Short-lived, single-use QR pairing codes. This is the one unauthenticated
-- endpoint that mints a session, so the code is 256 CSPRNG bits and comparison
-- is constant-time in application code.
CREATE TABLE pairing_codes (
    code       TEXT PRIMARY KEY,          -- 256-bit CSPRNG, base64url
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,      -- created_at + 2 minutes
    used_at    TIMESTAMPTZ
);

CREATE INDEX idx_pairing_codes_expires_at ON pairing_codes(expires_at);

-- Makes a client's retried write safe to repeat. A mobile client with an
-- offline queue will re-send requests whose response it never received;
-- without this, the second attempt debits the pantry twice.
--
-- Keyed by (key, user_id) so one client's key can never collide with another's.
-- request_hash is what separates a genuine replay from a client bug: the same
-- key with a different body is a 422, not a silent replay.
CREATE TABLE idempotency_records (
    key             TEXT NOT NULL,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    storage_id      UUID REFERENCES storages(id) ON DELETE CASCADE,
    request_hash    TEXT NOT NULL,        -- SHA-256 of method + path + body
    response_status INT NOT NULL,
    response_body   JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (key, user_id)
);

CREATE INDEX idx_idempotency_records_created_at ON idempotency_records(created_at);

-- Records deletions so a delta sync can tell a client a row is *gone*. A delta
-- built only from updated_at can never express a deletion, so without this a
-- deleted product lives in a client's cache forever.
--
-- entity_id is deliberately not a foreign key: the row it names has already
-- been deleted.
CREATE TABLE tombstones (
    id          UUID PRIMARY KEY,
    storage_id  UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    entity_type TEXT NOT NULL
                CHECK (entity_type IN ('product', 'category', 'location',
                                       'shopping_list', 'shopping_list_item')),
    entity_id   UUID NOT NULL,
    deleted_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_tombstones_storage_deleted ON tombstones(storage_id, deleted_at);

-- +goose Down
DROP TABLE IF EXISTS tombstones;
DROP TABLE IF EXISTS idempotency_records;
DROP TABLE IF EXISTS pairing_codes;
