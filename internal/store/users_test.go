package store_test

import (
	"context"
	"testing"
	"time"

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

// TestChangePasswordForAnUnknownUserChangesNothing is the failure half, and
// the reason this test file needs a database at all.
//
// It is what distinguishes one transaction from two sequential writes. The
// update matches no row, so the function returns ErrNotFound — and because
// the delete shares that transaction, the rollback means no session row was
// removed either. An implementation that revoked first, or that committed the
// update before attempting the delete, would leave evidence here: sessions
// gone for an id that named no account.
func TestChangePasswordForAnUnknownUserChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := requireDB(t)

	// A real user with a live session, then a reset aimed at an id that names
	// nobody. Nothing about the real user may move.
	userID := newUser(t, ctx)
	live, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)

	err = s.ChangePassword(ctx, newUUID(t), "$argon2id$nobody", "")
	require.ErrorIs(t, err, store.ErrNotFound)

	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE id = $1`, live.ID))
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM users WHERE password_hash = $1`, "$argon2id$nobody"),
		"a rolled-back change leaves no hash behind")
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
