package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// TestDeltaIsResumableUsesRetentionNotTheOldestTombstone is the regression for
// a rule that looked right and was not.
//
// The earlier predicate asked whether the client's cursor predated
// `min(deleted_at)` for that storage. That is unsound in the one case it
// matters: once the sweep has taken the only tombstone a storage ever had,
// `min(deleted_at)` is NULL and the rule answers "resumable" to a cursor from
// a year ago — handing back a delta that silently omits the deletion and
// leaving a deleted row in that client's cache indefinitely. Retention is the
// honest boundary because it does not depend on what happens to still be in
// the table: everything deleted inside the window is still recorded, and
// everything outside it may not be.
func TestDeltaIsResumableUsesRetentionNotTheOldestTombstone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	assert.True(t, store.DeltaIsResumable(now, now),
		"a cursor of 'right now' is always resumable")
	assert.True(t, store.DeltaIsResumable(now.Add(-29*24*time.Hour), now),
		"inside the window every deletion is still recorded")
	assert.True(t, store.DeltaIsResumable(now.Add(-store.TombstoneRetention), now),
		"the boundary itself is inclusive — nothing has been swept at exactly 30 days")
	assert.False(t, store.DeltaIsResumable(now.Add(-store.TombstoneRetention-time.Second), now),
		"one second past retention, a deletion may already have been swept")
	assert.False(t, store.DeltaIsResumable(now.Add(-365*24*time.Hour), now))

	// A cursor in the future is not a client to refuse: its delta is simply
	// empty. Refusing it would turn a few seconds of clock skew into a full
	// re-fetch of the whole catalog.
	assert.True(t, store.DeltaIsResumable(now.Add(time.Hour), now))
}

// TestStaleCursorIsRefusedEvenWithNoTombstonesLeft is the same bug seen from
// the database: a storage whose only tombstone has been swept must still refuse
// a cursor from before the window, because "no tombstones" and "no deletions"
// are not the same statement.
func TestStaleCursorIsRefusedEvenWithNoTombstonesLeft(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, _, _ := stocked(t, ctx, s, 1)
	require.NoError(t, s.DeleteProduct(ctx, storageID, productID))

	// Age the tombstone past retention and sweep it, exactly as the nightly
	// job would.
	_, err := execTest(ctx,
		`UPDATE tombstones SET deleted_at = now() - interval '45 days' WHERE entity_id = $1`, productID)
	require.NoError(t, err)
	_, err = s.SweepTombstones(ctx, time.Now())
	require.NoError(t, err)
	require.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM tombstones WHERE entity_id = $1`, productID),
		"the sweep took the only record of this deletion")

	_, err = s.ProductsChangedSince(ctx, storageID, timeZero())

	require.ErrorIs(t, err, store.ErrResyncRequired,
		"an empty tombstone table must not read as 'nothing was ever deleted'")
}

// TestProductDeltaReportsChangesAndDeletions is the ordinary path: what moved,
// what went, and a cursor to come back with.
func TestProductDeltaReportsChangesAndDeletions(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	kept, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)
	doomed, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Yoghurt"})
	require.NoError(t, err)

	// A cursor taken after both products exist and before anything happens to
	// them: the delta from here must be empty.
	cursor := timeNow(t, ctx)

	quiet, err := s.ProductsChangedSince(ctx, storageID, cursor)
	require.NoError(t, err)
	assert.Empty(t, quiet.Changed, "nothing has changed since the cursor")
	assert.Empty(t, quiet.Deleted)
	assert.False(t, quiet.SyncedAt.Before(cursor), "the sync point moves forward, never back")

	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy"})
	require.NoError(t, err)
	require.NoError(t, s.SetProductCategory(ctx, storageID, kept.ID, &category.ID))
	require.NoError(t, s.DeleteProduct(ctx, storageID, doomed.ID))

	delta, err := s.ProductsChangedSince(ctx, storageID, cursor)
	require.NoError(t, err)

	require.Len(t, delta.Changed, 1)
	assert.Equal(t, kept.ID, delta.Changed[0].ID, "the re-categorised product is in the delta")
	assert.Equal(t, []uuid.UUID{doomed.ID}, delta.Deleted,
		"a deletion reaches the client only through the tombstone")
}

// TestDeltaSyncPointIsUsableAsTheNextCursor closes the loop the whole feature
// depends on: feeding SyncedAt back must not skip a change, and must not repeat
// the whole list either.
func TestDeltaSyncPointIsUsableAsTheNextCursor(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	first, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)

	initial, err := s.ProductsChangedSince(ctx, storageID, timeNow(t, ctx).Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, initial.Changed, 1)

	second, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Yoghurt"})
	require.NoError(t, err)

	next, err := s.ProductsChangedSince(ctx, storageID, initial.SyncedAt)
	require.NoError(t, err)

	ids := make([]uuid.UUID, 0, len(next.Changed))
	for _, p := range next.Changed {
		ids = append(ids, p.ID)
	}
	assert.Contains(t, ids, second.ID, "the product created after the cursor must be in the next delta")
	assert.NotContains(t, ids, first.ID,
		"a product untouched since the cursor must not be sent again")
}

// TestDeltaIsScopedToOneStorage — the same rule as every other read here: a
// delta is not a way around storage scoping, and a neighbour's changes are not
// visible in it at any depth.
func TestDeltaIsScopedToOneStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	mine, theirs := twoStorages(t, ctx)

	cursor := timeNow(t, ctx)

	ours, err := s.CreateProduct(ctx, mine, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)
	foreign, err := s.CreateProduct(ctx, theirs, store.NewProduct{Name: "Their Butter"})
	require.NoError(t, err)
	require.NoError(t, s.DeleteProduct(ctx, theirs, foreign.ID))

	delta, err := s.ProductsChangedSince(ctx, mine, cursor)
	require.NoError(t, err)

	require.Len(t, delta.Changed, 1)
	assert.Equal(t, ours.ID, delta.Changed[0].ID)
	assert.Empty(t, delta.Deleted,
		"another storage's tombstone must not appear in this storage's delta")
}

// TestCategoryDeltaReportsAWholeDeletedSubtree is what lets a client hold these
// trees as a flat cache. Deleting a parent removes every node beneath it, and
// each of those ids has to reach the client separately — the client cannot
// derive the cascade, because it only learns the parent is gone.
func TestCategoryDeltaReportsAWholeDeletedSubtree(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	food, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	dairy, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy", ParentID: &food.ID})
	require.NoError(t, err)
	cheese, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Cheese", ParentID: &dairy.ID})
	require.NoError(t, err)

	cursor := timeNow(t, ctx)
	require.NoError(t, s.DeleteCategory(ctx, storageID, food.ID))

	delta, err := s.CategoriesChangedSince(ctx, storageID, cursor)
	require.NoError(t, err)

	assert.ElementsMatch(t, []uuid.UUID{food.ID, dairy.ID, cheese.ID}, delta.Deleted,
		"every node of the deleted subtree is tombstoned, not just the root")
	assert.Empty(t, delta.Changed, "deleted rows are gone, so they cannot also be 'changed'")
}

// TestLocationDeltaReportsAWholeDeletedSubtree is the location tree's copy of
// the rule above — the two trees are the same shape and fail the same way.
func TestLocationDeltaReportsAWholeDeletedSubtree(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)
	fridge, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge", ParentID: &kitchen.ID})
	require.NoError(t, err)

	cursor := timeNow(t, ctx)
	require.NoError(t, s.DeleteLocation(ctx, storageID, kitchen.ID))

	delta, err := s.LocationsChangedSince(ctx, storageID, cursor)
	require.NoError(t, err)

	assert.ElementsMatch(t, []uuid.UUID{kitchen.ID, fridge.ID}, delta.Deleted)
}

// TestDeltaDoesNotMixEntityKinds — each list endpoint answers for its own
// entity, so a deleted product must not turn up in the category delta's
// `deleted` array, where a client would drop a category it still has.
func TestDeltaDoesNotMixEntityKinds(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
	require.NoError(t, err)

	cursor := timeNow(t, ctx)
	require.NoError(t, s.DeleteProduct(ctx, storageID, product.ID))
	require.NoError(t, s.DeleteCategory(ctx, storageID, category.ID))
	require.NoError(t, s.DeleteLocation(ctx, storageID, location.ID))

	products, err := s.ProductsChangedSince(ctx, storageID, cursor)
	require.NoError(t, err)
	categories, err := s.CategoriesChangedSince(ctx, storageID, cursor)
	require.NoError(t, err)
	locations, err := s.LocationsChangedSince(ctx, storageID, cursor)
	require.NoError(t, err)

	assert.Equal(t, []uuid.UUID{product.ID}, products.Deleted)
	assert.Equal(t, []uuid.UUID{category.ID}, categories.Deleted)
	assert.Equal(t, []uuid.UUID{location.ID}, locations.Deleted)
}

// TestEveryDeltaKindRefusesAStaleCursor — the resync boundary is a property of
// delta sync, not of products, so all three loaders must enforce it. One that
// forgot would answer 200 with a quietly incomplete list.
func TestEveryDeltaKindRefusesAStaleCursor(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	ancient := time.Now().Add(-store.TombstoneRetention - 24*time.Hour)

	_, err := s.ProductsChangedSince(ctx, storageID, ancient)
	assert.ErrorIs(t, err, store.ErrResyncRequired, "products")

	_, err = s.CategoriesChangedSince(ctx, storageID, ancient)
	assert.ErrorIs(t, err, store.ErrResyncRequired, "categories")

	_, err = s.LocationsChangedSince(ctx, storageID, ancient)
	assert.ErrorIs(t, err, store.ErrResyncRequired, "locations")
}
