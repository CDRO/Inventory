-- +goose Up

-- Per-person, per-storage start page (docs/specs/34-navigation-and-start-page.md).
--
-- The column lives on storage_members because that row already means "this
-- person, this storage", which is exactly the key the preference has: two
-- members of one storage may choose differently, and one person may choose
-- differently for two storages. Removing someone from a storage takes the
-- preference with them, which is correct — there is nothing left to start on.
--
-- **This does not give storages roles.** storage_members still carries no role
-- and no rights (docs/specs/02-data-model.md,
-- docs/specs/03-auth-and-multi-tenancy.md); start_page grants nothing and is
-- never consulted by any authorization decision.
--
-- The CHECK is the backstop, not the validator: the closed list is written out
-- again in Go (internal/httpapi/membership.go) so an unknown value answers 422
-- with a field message rather than reaching the database and coming back as a
-- constraint violation, which the API would have to serve as a 500.
--
-- Pages that need a parameter to mean anything (review.html?job=,
-- stocktake.html?location=) are deliberately absent, and so is settings.html.
ALTER TABLE storage_members
    ADD COLUMN start_page TEXT NOT NULL DEFAULT 'dashboard'
    CHECK (start_page IN ('dashboard', 'inventory', 'products', 'locations',
                          'shopping_list', 'ingest', 'inbox'));

-- +goose Down

ALTER TABLE storage_members
    DROP COLUMN IF EXISTS start_page;
