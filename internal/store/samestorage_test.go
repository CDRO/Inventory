package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// The rules exercised here are the ones PostgreSQL cannot express. A foreign
// key proves a referenced row exists; it says nothing about which storage owns
// it. Every id arriving from a caller — a parent, a category, a location, a
// move target — must therefore be re-checked against the storage in the URL,
// and the failure must be indistinguishable from a nonexistent id
// (docs/specs/03-auth-and-multi-tenancy.md).
//
// These are exactly the invariants that pass a unit test suite while being
// broken, because nothing in the schema complains.

// twoStorages returns two storages that share nothing.
func twoStorages(t *testing.T, ctx context.Context) (uuid.UUID, uuid.UUID) {
	t.Helper()
	return newStorage(t, ctx), newStorage(t, ctx)
}

func TestCreateLocationRejectsParentFromAnotherStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	foreign, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Their Cellar"})
	require.NoError(t, err)

	_, err = s.CreateLocation(ctx, storageA, store.NewLocation{
		Name:     "Mine",
		ParentID: &foreign.ID,
	})

	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM locations WHERE storage_id = $1`, storageA),
		"the rejected insert must not have written a row")
}

// TestCrossStorageAndNonexistentAreIndistinguishable is the security property
// itself, not just a rejection.
//
// If a foreign id produced a different error from a made-up one, a user could
// probe ids and learn which ones name real rows in storages they cannot see —
// which is precisely the inference docs/specs/03-auth-and-multi-tenancy.md
// forbids. The two must be the same error, with the same message.
func TestCrossStorageAndNonexistentAreIndistinguishable(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	real, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Real, but theirs"})
	require.NoError(t, err)

	invented, err := uuid.NewV7()
	require.NoError(t, err)

	_, foreignErr := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "x", ParentID: &real.ID})
	_, missingErr := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "x", ParentID: &invented})

	require.ErrorIs(t, foreignErr, store.ErrNotFound)
	require.ErrorIs(t, missingErr, store.ErrNotFound)
	assert.Equal(t, missingErr.Error(), foreignErr.Error(),
		"a foreign id must not be distinguishable from a nonexistent one, even by message")
}

func TestMoveLocationRejectsForeignEnds(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	mine, err := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "Mine"})
	require.NoError(t, err)
	theirs, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)

	t.Run("target parent in another storage", func(t *testing.T) {
		err := s.MoveLocation(ctx, storageA, mine.ID, &theirs.ID)
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("node in another storage", func(t *testing.T) {
		err := s.MoveLocation(ctx, storageA, theirs.ID, nil)
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	// Neither attempt may have moved anything.
	var parent *uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT parent_id FROM locations WHERE id = $1`, theirs.ID).Scan(&parent))
	assert.Nil(t, parent)
}

func TestCreateProductRejectsCategoryFromAnotherStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	foreignCat, err := s.CreateCategory(ctx, storageB, store.NewCategory{Name: "Their Dairy"})
	require.NoError(t, err)

	_, err = s.CreateProduct(ctx, storageA, store.NewProduct{
		Name:       "Milk",
		CategoryID: &foreignCat.ID,
	})

	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM products WHERE storage_id = $1`, storageA))
}

func TestSetProductCategoryRejectsForeignCategory(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	product, err := s.CreateProduct(ctx, storageA, store.NewProduct{Name: "Rice"})
	require.NoError(t, err)
	foreignCat, err := s.CreateCategory(ctx, storageB, store.NewCategory{Name: "Their Grains"})
	require.NoError(t, err)

	err = s.SetProductCategory(ctx, storageA, product.ID, &foreignCat.ID)

	require.ErrorIs(t, err, store.ErrNotFound)
	var category *uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT category_id FROM products WHERE id = $1`, product.ID).Scan(&category))
	assert.Nil(t, category, "the product must keep its own category")
}

// TestCreateBatchRejectsForeignLocation is the one with physical consequences:
// inventory_batches.location_id is a plain foreign key, so without the
// application check a batch could be filed against a shelf in somebody else's
// house — and it would show up in their per-location analytics.
func TestCreateBatchRejectsForeignLocation(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	product, err := s.CreateProduct(ctx, storageA, store.NewProduct{Name: "Passata"})
	require.NoError(t, err)
	foreignLoc, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Their Pantry"})
	require.NoError(t, err)

	_, err = s.CreateBatch(ctx, storageA, store.NewBatch{
		ProductID:  product.ID,
		LocationID: foreignLoc.ID,
		Quantity:   3,
		Reason:     store.ReasonPurchase,
	})

	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE location_id = $1`, foreignLoc.ID))
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1`, product.ID),
		"a rejected batch must not leave a log row behind")
}

func TestCreateBatchRejectsForeignProduct(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	foreignProduct, err := s.CreateProduct(ctx, storageB, store.NewProduct{Name: "Their Beans"})
	require.NoError(t, err)
	mine, err := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "My Shelf"})
	require.NoError(t, err)

	_, err = s.CreateBatch(ctx, storageA, store.NewBatch{
		ProductID:  foreignProduct.ID,
		LocationID: mine.ID,
		Quantity:   1,
		Reason:     store.ReasonPurchase,
	})

	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestSplitBatchRejectsForeignTargetLocation(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	product, err := s.CreateProduct(ctx, storageA, store.NewProduct{Name: "Jars"})
	require.NoError(t, err)
	cellar, err := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "Cellar"})
	require.NoError(t, err)
	batch, err := s.CreateBatch(ctx, storageA, store.NewBatch{
		ProductID: product.ID, LocationID: cellar.ID, Quantity: 3, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	foreignKitchen, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Their Kitchen"})
	require.NoError(t, err)

	_, err = s.SplitBatch(ctx, storageA, batch.ID, 1, foreignKitchen.ID, nil)

	require.ErrorIs(t, err, store.ErrNotFound)

	// The whole split must have rolled back: the source keeps its three units
	// and no move was logged.
	var quantity int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT quantity FROM inventory_batches WHERE id = $1`, batch.ID).Scan(&quantity))
	assert.Equal(t, 3, quantity, "a rejected split must not debit the source")
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE reason = 'move' AND product_id = $1`, product.ID))
}

func TestAdjustBatchRejectsBatchFromAnotherStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	product, err := s.CreateProduct(ctx, storageB, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)
	loc, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Their Shelf"})
	require.NoError(t, err)
	batch, err := s.CreateBatch(ctx, storageB, store.NewBatch{
		ProductID: product.ID, LocationID: loc.ID, Quantity: 5, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	// Storage A knows the id — ids are guessable in a report, a screenshot, a
	// shared link — and must still be refused.
	err = s.AdjustBatch(ctx, storageA, batch.ID, -5, store.ReasonConsumption, nil)

	require.ErrorIs(t, err, store.ErrNotFound)
	var quantity int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT quantity FROM inventory_batches WHERE id = $1`, batch.ID).Scan(&quantity))
	assert.Equal(t, 5, quantity, "another storage must not be able to consume this stock")
}

func TestDeleteRejectsRowsFromAnotherStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	loc, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)
	cat, err := s.CreateCategory(ctx, storageB, store.NewCategory{Name: "Theirs"})
	require.NoError(t, err)
	prod, err := s.CreateProduct(ctx, storageB, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)

	assert.ErrorIs(t, s.DeleteLocation(ctx, storageA, loc.ID), store.ErrNotFound)
	assert.ErrorIs(t, s.DeleteCategory(ctx, storageA, cat.ID), store.ErrNotFound)
	assert.ErrorIs(t, s.DeleteProduct(ctx, storageA, prod.ID), store.ErrNotFound)

	assert.Equal(t, 3, countRows(t, ctx,
		`SELECT (SELECT count(*) FROM locations WHERE id = $1)
		      + (SELECT count(*) FROM categories WHERE id = $2)
		      + (SELECT count(*) FROM products WHERE id = $3)`, loc.ID, cat.ID, prod.ID),
		"nothing belonging to the other storage may be deleted")

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM tombstones WHERE storage_id = $1`, storageA),
		"a refused delete must not record a tombstone")
}

// TestTreeQueriesNeverLeakAnotherStorage covers the read side. A tree query
// that forgot its storage_id filter would splice another household's shelves
// into this one's picker.
func TestTreeQueriesNeverLeakAnotherStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	_, err := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "Mine"})
	require.NoError(t, err)
	_, err = s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)
	_, err = s.CreateCategory(ctx, storageA, store.NewCategory{Name: "Mine"})
	require.NoError(t, err)
	_, err = s.CreateCategory(ctx, storageB, store.NewCategory{Name: "Theirs"})
	require.NoError(t, err)

	locations, err := s.LocationTree(ctx, storageA)
	require.NoError(t, err)
	require.Len(t, locations, 1)
	assert.Equal(t, "Mine", locations[0].Name)
	assert.Equal(t, storageA, locations[0].StorageID)

	categories, err := s.CategoryTree(ctx, storageA)
	require.NoError(t, err)
	require.Len(t, categories, 1)
	assert.Equal(t, "Mine", categories[0].Name)
	assert.Equal(t, storageA, categories[0].StorageID)
}

// TestUpdateLocationRejectsForeignEnds is the acceptance criterion "any
// location id from another storage — as a path parameter, a parent_id, a move
// target, or a confirm-body field — yields 404" on the combined patch path.
//
// Both ends are covered because both are ways in: the node being patched, and
// the parent it is being pointed at.
func TestUpdateLocationRejectsForeignEnds(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	mine, err := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "Mine"})
	require.NoError(t, err)
	theirs, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)

	t.Run("a parent in another storage", func(t *testing.T) {
		_, err := s.UpdateLocation(ctx, storageA, mine.ID, store.LocationPatch{
			ParentID: &theirs.ID, SetParentID: true,
		})
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("a node in another storage", func(t *testing.T) {
		name := "Mine Now"
		_, err := s.UpdateLocation(ctx, storageA, theirs.ID, store.LocationPatch{Name: &name})
		assert.ErrorIs(t, err, store.ErrNotFound)

		var stored string
		require.NoError(t, testPool.QueryRow(ctx,
			`SELECT name FROM locations WHERE id = $1`, theirs.ID).Scan(&stored))
		assert.Equal(t, "Theirs", stored, "the refusal must also not have written anything")
	})

	t.Run("a nonexistent id is the same error", func(t *testing.T) {
		name := "x"
		_, foreignErr := s.UpdateLocation(ctx, storageA, theirs.ID, store.LocationPatch{Name: &name})
		_, missingErr := s.UpdateLocation(ctx, storageA, uuid.New(), store.LocationPatch{Name: &name})

		assert.Equal(t, foreignErr.Error(), missingErr.Error(),
			"a caller must not be able to tell a real row in another storage from no row at all")
	})
}

// TestMoveBatchRejectsForeignEnds covers the whole-batch move the same way
// TestSplitBatchRejectsForeignTargetLocation covers the split: a batch or a
// target location belonging to somebody else is a not-found, not a refusal that
// admits the row exists.
func TestMoveBatchRejectsForeignEnds(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageB := newStorage(t, ctx)

	storageA, _, _, batchID := stocked(t, ctx, s, 4)
	mineElsewhere, err := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)
	foreign, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Their Kitchen"})
	require.NoError(t, err)

	t.Run("a target location in another storage", func(t *testing.T) {
		_, err := s.MoveBatch(ctx, storageA, batchID, foreign.ID, nil)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("a batch in another storage", func(t *testing.T) {
		_, err := s.MoveBatch(ctx, storageB, batchID, foreign.ID, nil)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// Nothing above may have moved the batch or written a ledger row.
	var locationID uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT location_id FROM inventory_batches WHERE id = $1`, batchID).Scan(&locationID))
	assert.NotEqual(t, foreign.ID, locationID)
	assert.NotEqual(t, mineElsewhere.ID, locationID)
}
