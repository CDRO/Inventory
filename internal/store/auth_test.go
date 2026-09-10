package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// TestIsAdminReflectsTheDatabaseOnEveryCall is the data-layer half of the
// invariant that admin rights are re-read per request.
//
// The flag is changed here **behind the store's back**, through the raw test
// pool, and the very next call on the same *Store — same connection pool, same
// process — must see it. Any caching at all fails this: a value kept on a
// session row, memoised per user, or read once at login. The consequence of
// caching is that revoking someone's admin rights does nothing until their
// session happens to expire.
//
// The HTTP half — that RequireAdmin calls this on every single request rather
// than trusting something carried in the request — lands with the spec 04 work.
func TestIsAdminReflectsTheDatabaseOnEveryCall(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	// Starts as an ordinary user.
	isAdmin, err := s.IsAdmin(ctx, userID)
	require.NoError(t, err)
	require.False(t, isAdmin)

	// Promote out-of-band, then demote again, checking after each change.
	for _, want := range []bool{true, false, true, false} {
		_, err := execTest(ctx, `UPDATE users SET is_admin = $1 WHERE id = $2`, want, userID)
		require.NoError(t, err)

		got, err := s.IsAdmin(ctx, userID)
		require.NoError(t, err)
		assert.Equalf(t, want, got,
			"IsAdmin must re-read the database; expected %v after an out-of-band change", want)
	}
}

// TestIsAdminIsFalseForAUserThatNoLongerExists — a race between deletion and a
// request must end in refusal, not in an error a handler might mistake for a
// server fault.
func TestIsAdminIsFalseForAUserThatNoLongerExists(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	require.NoError(t, s.SetAdmin(ctx, userID, true))
	require.NoError(t, s.DeleteUser(ctx, userID))

	isAdmin, err := s.IsAdmin(ctx, userID)

	require.NoError(t, err)
	assert.False(t, isAdmin)
}

// TestDeletingAUserRevokesTheirSessionsImmediately — spec 03 requires that
// deleting a user takes their access away at once. Sessions outliving the
// account would make the deletion cosmetic until they expired on their own.
func TestDeletingAUserRevokesTheirSessionsImmediately(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	victim := newUser(t, ctx)
	bystander := newUser(t, ctx)

	var victimSessions []string
	for i := 0; i < 3; i++ {
		session, err := s.CreateSession(ctx, victim, store.SessionBrowser, nil, time.Hour)
		require.NoError(t, err)
		victimSessions = append(victimSessions, session.ID)
	}
	othersSession, err := s.CreateSession(ctx, bystander, store.SessionDevice, ptrString("Pixel 9"), time.Hour)
	require.NoError(t, err)

	require.NoError(t, s.DeleteUser(ctx, victim))

	for _, id := range victimSessions {
		_, err := s.LookupSession(ctx, id)
		assert.ErrorIsf(t, err, store.ErrNotFound,
			"session %s must stop working the moment the account goes", id)
	}
	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, victim))

	still, err := s.LookupSession(ctx, othersSession.ID)
	require.NoError(t, err, "another user's session must be untouched")
	assert.Equal(t, othersSession.ID, still.ID)
}

func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	username := "dup-" + randomSuffix()
	_, err := s.CreateUser(ctx, store.NewUser{Username: username, PasswordHash: "x", DisplayName: "First"})
	require.NoError(t, err)

	_, err = s.CreateUser(ctx, store.NewUser{Username: username, PasswordHash: "y", DisplayName: "Second"})

	require.ErrorIs(t, err, store.ErrDuplicate)
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM users WHERE username = $1`, username))

	// The first account is untouched: a rejected duplicate must not overwrite.
	var hash, display string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT password_hash, display_name FROM users WHERE username = $1`, username).Scan(&hash, &display))
	assert.Equal(t, "x", hash)
	assert.Equal(t, "First", display)
}

// TestIsStorageMemberCannotDistinguishMissingFromInaccessible is the
// data-layer half of the 404-not-403 rule.
//
// A storage that does not exist and a storage the caller is not in must both
// answer false, through the same code path, so nothing downstream is able to
// tell them apart. The HTTP half — a byte-identical 404 rather than a 403 —
// belongs to the spec 04 work.
func TestIsStorageMemberCannotDistinguishMissingFromInaccessible(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	member := newUser(t, ctx)
	outsider := newUser(t, ctx)
	storageID := newStorage(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, member))

	invented := newUUID(t)

	yes, err := s.IsStorageMember(ctx, storageID, member)
	require.NoError(t, err)
	assert.True(t, yes)

	inaccessible, err := s.IsStorageMember(ctx, storageID, outsider)
	require.NoError(t, err)

	nonexistent, err := s.IsStorageMember(ctx, invented, outsider)
	require.NoError(t, err)

	assert.False(t, inaccessible, "a non-member has no access")
	assert.False(t, nonexistent, "an unknown storage has no access")
	assert.Equal(t, nonexistent, inaccessible,
		"the two must be the same answer; a difference here is what a prober would measure")
}

func TestAddMemberIsIdempotent(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	userID := newUser(t, ctx)
	storageID := newStorage(t, ctx)

	require.NoError(t, s.AddMember(ctx, storageID, userID))
	require.NoError(t, s.AddMember(ctx, storageID, userID),
		"re-adding a member is the state the admin asked for")

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM storage_members WHERE storage_id = $1 AND user_id = $2`, storageID, userID))
}

// TestRemoveMemberRevokesAccessButNotSessions — membership is not a
// credential. The user may belong to other storages, so signing them out
// everywhere because one household removed them would be wrong.
func TestRemoveMemberRevokesAccessButNotSessions(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	userID := newUser(t, ctx)
	kept := newStorage(t, ctx)
	revoked := newStorage(t, ctx)
	require.NoError(t, s.AddMember(ctx, kept, userID))
	require.NoError(t, s.AddMember(ctx, revoked, userID))

	session, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)

	require.NoError(t, s.RemoveMember(ctx, revoked, userID))

	gone, err := s.IsStorageMember(ctx, revoked, userID)
	require.NoError(t, err)
	assert.False(t, gone)

	remains, err := s.IsStorageMember(ctx, kept, userID)
	require.NoError(t, err)
	assert.True(t, remains, "removal from one storage must not affect another")

	_, err = s.LookupSession(ctx, session.ID)
	assert.NoError(t, err, "the session survives; only the membership went")
}

func TestRemoveMemberReportsANonMember(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	err := s.RemoveMember(ctx, newStorage(t, ctx), newUser(t, ctx))

	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestStoragesForUserListsOnlyOwnMemberships is what GET /api/auth/me is built
// from. It must be filtered by membership rather than listing storages and
// marking which are reachable — the latter leaks their existence.
func TestStoragesForUserListsOnlyOwnMemberships(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	me := newUser(t, ctx)
	someoneElse := newUser(t, ctx)

	mine := newStorage(t, ctx)
	alsoMine := newStorage(t, ctx)
	theirs := newStorage(t, ctx)

	require.NoError(t, s.AddMember(ctx, mine, me))
	require.NoError(t, s.AddMember(ctx, alsoMine, me))
	require.NoError(t, s.AddMember(ctx, theirs, someoneElse))

	got, err := s.StoragesForUser(ctx, me)
	require.NoError(t, err)

	var ids []string
	for _, storage := range got {
		ids = append(ids, storage.ID.String())
	}
	assert.ElementsMatch(t, []string{mine.String(), alsoMine.String()}, ids)
	assert.NotContains(t, ids, theirs.String(), "a storage the caller is not in must not appear")
}

// TestStoragesForUserIsEmptyForANewUser — a user an admin has not added
// anywhere is a normal state, not an error, and must not hint that storages
// exist which they cannot see.
func TestStoragesForUserIsEmptyForANewUser(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	// Storages exist in the system; this user is simply in none of them.
	newStorage(t, ctx)
	newStorage(t, ctx)

	got, err := s.StoragesForUser(ctx, newUser(t, ctx))

	require.NoError(t, err, "belonging to nothing is not a failure")
	assert.Empty(t, got)
}

// TestAdminRightsGrantNoStorageAccess — is_admin is orthogonal to membership.
// An admin who wants to use a storage must be added to it like anyone else.
func TestAdminRightsGrantNoStorageAccess(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	adminID := newUser(t, ctx)
	require.NoError(t, s.SetAdmin(ctx, adminID, true))
	storageID := newStorage(t, ctx)

	isAdmin, err := s.IsAdmin(ctx, adminID)
	require.NoError(t, err)
	require.True(t, isAdmin)

	member, err := s.IsStorageMember(ctx, storageID, adminID)
	require.NoError(t, err)
	assert.False(t, member, "being an admin is not membership")

	storages, err := s.StoragesForUser(ctx, adminID)
	require.NoError(t, err)
	assert.Empty(t, storages,
		"an admin's storage list is their own memberships, not every storage")
}

func TestListMembersReturnsTheStoragesOwn(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageA := newStorage(t, ctx)
	storageB := newStorage(t, ctx)
	alice := newUser(t, ctx)
	bob := newUser(t, ctx)
	carol := newUser(t, ctx)

	require.NoError(t, s.AddMember(ctx, storageA, alice))
	require.NoError(t, s.AddMember(ctx, storageA, bob))
	require.NoError(t, s.AddMember(ctx, storageB, carol))

	members, err := s.ListMembers(ctx, storageA)
	require.NoError(t, err)

	var ids []string
	for _, m := range members {
		ids = append(ids, m.UserID.String())
		assert.NotEmpty(t, m.Username)
	}
	assert.ElementsMatch(t, []string{alice.String(), bob.String()}, ids)
}

// TestDeletingAStorageTakesItsMembershipsWithIt — otherwise a membership row
// would outlive the thing it grants access to.
func TestDeletingAStorageTakesItsMembershipsWithIt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	userID := newUser(t, ctx)
	storageID := newStorage(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, userID))

	require.NoError(t, s.DeleteStorage(ctx, storageID))

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM storage_members WHERE storage_id = $1`, storageID))

	member, err := s.IsStorageMember(ctx, storageID, userID)
	require.NoError(t, err)
	assert.False(t, member)
}
