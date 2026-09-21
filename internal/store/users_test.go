package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// The credential-lifecycle writes of docs/specs/14-account-self-service.md.
//
// These run against a real PostgreSQL, because the guarantee under test is a
// transactional one: the httpapi tests use an in-memory fake, which can only
// show that the handlers ask for the right thing, never that the two writes
// actually land together.

// TestChangePasswordKeepsOnlyTheNamedSession — the self-service change. Every
// other session of that user goes, whatever kind it is; the one that made the
// request survives.
func TestChangePasswordKeepsOnlyTheNamedSession(t *testing.T) {
	ctx := context.Background()
	s := requireDB(t)

	userID := newUser(t, ctx)
	other := newUser(t, ctx)

	keep, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)
	laptop, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)
	phone, err := s.CreateSession(ctx, userID, store.SessionDevice, ptrString("Phone"), time.Hour)
	require.NoError(t, err)
	// A bystander's session, to prove the delete is scoped to one user.
	bystander, err := s.CreateSession(ctx, other, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)

	require.NoError(t, s.ChangePassword(ctx, userID, "$argon2id$new", keep.ID))

	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE id = $1`, keep.ID),
		"the session that made the change survives")
	for _, gone := range []*store.Session{laptop, phone} {
		assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE id = $1`, gone.ID),
			"every other session of that user is revoked, browser and device alike")
	}
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE id = $1`, bystander.ID),
		"another user's sessions are untouched")

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM users WHERE id = $1 AND password_hash = $2`, userID, "$argon2id$new"),
		"and the hash really was replaced")
}

// TestChangePasswordWithNoKeptSessionRevokesAll — the admin reset. The
// resetter cannot know which of the target's sessions are legitimate, so none
// of them are.
func TestChangePasswordWithNoKeptSessionRevokesAll(t *testing.T) {
	ctx := context.Background()
	s := requireDB(t)

	userID := newUser(t, ctx)
	for range 3 {
		_, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
		require.NoError(t, err)
	}

	require.NoError(t, s.ChangePassword(ctx, userID, "$argon2id$reset", ""))

	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, userID),
		"an empty keepSessionID keeps nothing")
}

// TestChangePasswordForAnUnknownUserIsNotFound — an id that names no account
// is refused, and writes nothing.
//
// This says nothing about transactionality, deliberately: the delete is
// scoped by user_id, so an id nobody owns could not have touched another
// user's sessions however the writes were ordered. The transaction guarantee
// is the next test's job.
func TestChangePasswordForAnUnknownUserIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := requireDB(t)

	err := s.ChangePassword(ctx, newUUID(t), "$argon2id$nobody", "")
	require.ErrorIs(t, err, store.ErrNotFound)

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM users WHERE password_hash = $1`, "$argon2id$nobody"),
		"a refused change leaves no hash behind")
}

// TestChangePasswordRollsBackTheHashWhenRevocationFails is the test that
// actually distinguishes one transaction from two sequential writes.
//
// The dangerous case is not an id that names nobody. It is an existing user
// whose sessions fail to go: if the hash update had already committed by
// then, the account would be left with a new password and every
// previously-stolen session still working — the lock changed and the windows
// open, which is the precise state docs/specs/14-account-self-service.md
// requires to be impossible, and which issue #90 calls out as "not as a
// best-effort follow-up".
//
// Forcing that failure is what the trigger below is for: it makes DELETE on
// this one user's session rows raise, so the revocation fails *after* the
// update has run. The assertion is that the update did not survive it. Run
// against two unguarded sequential statements, this test fails — the hash
// would be the new one and the session would still be live.
func TestChangePasswordRollsBackTheHashWhenRevocationFails(t *testing.T) {
	ctx := context.Background()
	s := requireDB(t)

	userID := newUser(t, ctx)
	session, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)

	before, err := s.UserByID(ctx, userID)
	require.NoError(t, err)

	blockSessionDeletes(t, ctx, userID)

	err = s.ChangePassword(ctx, userID, "$argon2id$must-not-survive", "")
	require.Error(t, err, "a revocation that fails must fail the whole change")

	after, err := s.UserByID(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, before.PasswordHash, after.PasswordHash,
		"the hash update must roll back with the revocation it is paired with")
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE id = $1`, session.ID),
		"and the session is still live, so the account is exactly as it was")
}

// blockSessionDeletes installs a trigger that makes deleting one user's
// session rows raise, and removes it when the test ends.
//
// Row-level and pinned to a single user id by its WHEN clause, so it cannot
// affect any other row in the shared throwaway database while it exists. The
// identifiers are interpolated rather than bound because PostgreSQL takes no
// parameters in DDL; both values are derived from a UUID this test generated,
// so there is nothing external in the string.
func blockSessionDeletes(t *testing.T, ctx context.Context, userID uuid.UUID) {
	t.Helper()

	// Prefixed, because an identifier may not begin with a digit and a UUID
	// often does.
	fn := "block_session_delete_" + strings.ReplaceAll(userID.String(), "-", "")
	trigger := "trg_" + fn

	_, err := execTest(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS
		$fn$ BEGIN RAISE EXCEPTION 'session delete blocked for test'; END $fn$`, fn))
	require.NoError(t, err)

	_, err = execTest(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE DELETE ON sessions
		 FOR EACH ROW WHEN (OLD.user_id = '%s'::uuid)
		 EXECUTE FUNCTION %s()`, trigger, userID, fn))
	require.NoError(t, err)

	t.Cleanup(func() {
		// A fresh context: the test's own may already be done with.
		clean := context.Background()
		_, _ = execTest(clean, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON sessions`, trigger))
		_, _ = execTest(clean, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

// TestChangePasswordIsVisibleToTheNextLookup — the write is committed, not
// left in an open transaction the next reader cannot see.
func TestChangePasswordIsVisibleToTheNextLookup(t *testing.T) {
	ctx := context.Background()
	s := requireDB(t)

	userID := newUser(t, ctx)
	require.NoError(t, s.ChangePassword(ctx, userID, "$argon2id$committed", ""))

	user, err := s.UserByID(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "$argon2id$committed", user.PasswordHash)
}

// TestSetDisplayNameRenamesOnlyTheDisplayName — usernames are immutable, and
// the returned row is the updated one rather than a stale read.
func TestSetDisplayNameRenamesOnlyTheDisplayName(t *testing.T) {
	ctx := context.Background()
	s := requireDB(t)

	userID := newUser(t, ctx)
	before, err := s.UserByID(ctx, userID)
	require.NoError(t, err)

	updated, err := s.SetDisplayName(ctx, userID, "Renamed")
	require.NoError(t, err)

	assert.Equal(t, "Renamed", updated.DisplayName)
	assert.Equal(t, before.Username, updated.Username, "the login identifier does not move")
	assert.Equal(t, before.PasswordHash, updated.PasswordHash)
	assert.Equal(t, before.IsAdmin, updated.IsAdmin)

	reread, err := s.UserByID(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", reread.DisplayName)
}

// TestSetDisplayNameForAnUnknownUserIsNotFound — so the handler can map it to
// the same 404 everything else unknown gets.
func TestSetDisplayNameForAnUnknownUserIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := requireDB(t)

	_, err := s.SetDisplayName(ctx, newUUID(t), "Nobody")
	assert.ErrorIs(t, err, store.ErrNotFound)
}
