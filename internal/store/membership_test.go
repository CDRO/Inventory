package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// storage_members.start_page — the per-person, per-storage start page of
// docs/specs/34-navigation-and-start-page.md.
//
// These run against a real database because the guarantees are the query's and
// the column's, not the handler's: whether the join is filtered by user id,
// whether the UPDATE's WHERE names both halves of the primary key, and whether
// the CHECK is actually on the column. None of that is observable through a
// fake.

// TestStartPageDefaultsToTheDashboard — a membership nobody has touched reads
// as the column default, so a fresh member lands somewhere real.
func TestStartPageDefaultsToTheDashboard(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	userID := newUser(t, ctx)
	storageID := newStorage(t, ctx)
	require.NoError(t, s.AddMember(ctx, store.SystemActor, storageID, userID))

	got, err := s.StorageMembershipsForUser(ctx, userID)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, storageID, got[0].ID)
	assert.Equal(t, "dashboard", got[0].StartPage)
}

// TestStartPageIsPerMemberNotPerStorage is the isolation claim at the level it
// is actually made: two members of one storage, two different values, and each
// read returning only its own.
//
// A join missing `WHERE m.user_id = $1` returns one row per storage either
// way — the same count, the same names, and whichever member's preference the
// planner reached. That is the failure this exists to catch, and it is
// invisible with one member.
func TestStartPageIsPerMemberNotPerStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	bob := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, store.SystemActor, storageID, alice))
	require.NoError(t, s.AddMember(ctx, store.SystemActor, storageID, bob))

	require.NoError(t, s.SetStartPage(ctx, storageID, alice, "inventory"))
	require.NoError(t, s.SetStartPage(ctx, storageID, bob, "ingest"))

	hers, err := s.StorageMembershipsForUser(ctx, alice)
	require.NoError(t, err)
	require.Len(t, hers, 1)
	assert.Equal(t, "inventory", hers[0].StartPage)

	his, err := s.StorageMembershipsForUser(ctx, bob)
	require.NoError(t, err)
	require.Len(t, his, 1)
	assert.Equal(t, "ingest", his[0].StartPage, "the same storage, the other member's own choice")
}

// TestStartPageIsPerStorageNotPerUser is the other axis of the same key: one
// person, two storages, two different choices. "When I open *this* household,
// show me *this*" is only true if both halves of the key are used.
func TestStartPageIsPerStorageNotPerUser(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	userID := newUser(t, ctx)
	home := newStorage(t, ctx)
	cabin := newStorage(t, ctx)
	require.NoError(t, s.AddMember(ctx, store.SystemActor, home, userID))
	require.NoError(t, s.AddMember(ctx, store.SystemActor, cabin, userID))

	require.NoError(t, s.SetStartPage(ctx, home, userID, "inbox"))
	require.NoError(t, s.SetStartPage(ctx, cabin, userID, "locations"))

	got, err := s.StorageMembershipsForUser(ctx, userID)
	require.NoError(t, err)

	byStorage := map[string]string{}
	for _, m := range got {
		byStorage[m.ID.String()] = m.StartPage
	}
	assert.Equal(t, "inbox", byStorage[home.String()])
	assert.Equal(t, "locations", byStorage[cabin.String()])
}

// TestStorageMembershipsForUserListsOnlyOwnMemberships mirrors
// TestStoragesForUserListsOnlyOwnMemberships for the method that replaced it
// in GET /api/auth/me. The non-enumeration rule is not weakened by adding a
// column: a storage the caller is not in must still not appear at all.
func TestStorageMembershipsForUserListsOnlyOwnMemberships(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	me := newUser(t, ctx)
	someoneElse := newUser(t, ctx)

	mine := newStorage(t, ctx)
	theirs := newStorage(t, ctx)
	require.NoError(t, s.AddMember(ctx, store.SystemActor, mine, me))
	require.NoError(t, s.AddMember(ctx, store.SystemActor, theirs, someoneElse))

	got, err := s.StorageMembershipsForUser(ctx, me)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, mine, got[0].ID)
}

// TestSetStartPageWritesNoOtherMembersRow — the UPDATE names both halves of
// the primary key. A WHERE on storage_id alone would set every member of the
// household to one person's preference, and the household would have no way to
// tell that from "it saved".
func TestSetStartPageWritesNoOtherMembersRow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	me := newUser(t, ctx)
	housemate := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, store.SystemActor, storageID, me))
	require.NoError(t, s.AddMember(ctx, store.SystemActor, storageID, housemate))

	require.NoError(t, s.SetStartPage(ctx, storageID, me, "products"))

	theirs, err := s.StorageMembershipsForUser(ctx, housemate)
	require.NoError(t, err)
	require.Len(t, theirs, 1)
	assert.Equal(t, "dashboard", theirs[0].StartPage, "untouched")
}

// TestSetStartPageReportsANonMember — no row matched, so nothing was written
// and the caller is told so with the error every "not yours" maps to a 404.
func TestSetStartPageReportsANonMember(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	err := s.SetStartPage(ctx, newStorage(t, ctx), newUser(t, ctx), "inventory")

	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestRemovingAMemberTakesTheStartPageWithThem — the preference lives on the
// membership row, so losing access loses the preference. That is correct:
// there is nothing left to start on, and a re-added member starts from the
// default rather than from a choice they made before they were removed.
func TestRemovingAMemberTakesTheStartPageWithThem(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, store.SystemActor, storageID, userID))
	require.NoError(t, s.SetStartPage(ctx, storageID, userID, "shopping_list"))

	require.NoError(t, s.RemoveMember(ctx, store.SystemActor, storageID, userID))
	require.NoError(t, s.AddMember(ctx, store.SystemActor, storageID, userID))

	got, err := s.StorageMembershipsForUser(ctx, userID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "dashboard", got[0].StartPage)
}

// TestStartPageColumnRefusesAValueOutsideTheList proves the CHECK is really on
// the column and not merely written in the migration's comment.
//
// The API validates before it gets here (internal/httpapi/membership.go), so
// this path is the backstop: anything reaching the column by another road —
// a restore, a hand-run UPDATE, a future writer — is refused by the database
// rather than leaving a value no page can route.
func TestStartPageColumnRefusesAValueOutsideTheList(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, store.SystemActor, storageID, userID))

	err := s.SetStartPage(ctx, storageID, userID, "settings")

	require.Error(t, err, "the column's CHECK must refuse a page outside the closed list")
	assert.NotErrorIs(t, err, store.ErrNotFound, "the row exists; the value is what was wrong")
}
