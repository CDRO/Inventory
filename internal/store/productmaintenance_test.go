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

// The maintenance surface of docs/specs/16-product-maintenance.md: the full
// product edit (including the default_shelf_life_days endpoint spec 08 noted
// as missing), the duplicate merge, and the delete.
//
// The properties worth testing here are the ones that pass silently when they
// are broken: which expiry dates a cascade is allowed to touch, whether a
// merge invents ledger rows, and whether the catalog — an insert-only table
// (docs/specs/02-data-model.md) — is left alone by operations that delete
// products.

// mergeFixture is two products of one storage, each with stock, plus the
// location they sit in.
type mergeFixture struct {
	storageID uuid.UUID
	survivor  *store.Product
	source    *store.Product
	location  uuid.UUID
}

func newMergeFixture(t *testing.T, ctx context.Context, s *store.Store) mergeFixture {
	t.Helper()

	storageID := newStorage(t, ctx)
	survivor, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Barilla Penne"})
	require.NoError(t, err)
	source, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Penne Barilla 500g"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	return mergeFixture{storageID: storageID, survivor: survivor, source: source, location: location.ID}
}

func (f mergeFixture) stock(t *testing.T, ctx context.Context, s *store.Store, productID uuid.UUID, qty int) uuid.UUID {
	t.Helper()

	batch, err := s.CreateBatch(ctx, f.storageID, store.NewBatch{
		ProductID: productID, LocationID: f.location, Quantity: qty, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	return batch.ID
}

func batchProduct(t *testing.T, ctx context.Context, batchID uuid.UUID) uuid.UUID {
	t.Helper()

	var productID uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT product_id FROM inventory_batches WHERE id = $1`, batchID).Scan(&productID))
	return productID
}

// TestUpdateProductShelfLifeRecomputesDerivedOnly is the endpoint
// docs/specs/08-expiration-and-classification.md records as missing: setting
// products.default_shelf_life_days has to move the dates that follow from it,
// report how many moved, and leave every date a person typed alone.
func TestUpdateProductShelfLifeRecomputesDerivedOnly(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	derived := f.stock(t, ctx, s, f.survivor.ID, 2)
	typed := f.stock(t, ctx, s, f.survivor.ID, 1)

	chosen := day(2031, time.March, 1)
	_, err := s.SetBatchExpiration(ctx, f.storageID, typed, chosen, nil)
	require.NoError(t, err)

	updated, recomputed, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		DefaultShelfLifeDays: ptrInt(30), SetDefaultShelfLifeDays: true,
	})
	require.NoError(t, err)
	require.NotNil(t, updated.DefaultShelfLifeDays)
	assert.Equal(t, 30, *updated.DefaultShelfLifeDays)
	assert.Equal(t, 1, recomputed, "exactly the one derived batch is recomputed")

	date, source := batchExpiry(t, ctx, derived)
	assert.Equal(t, "derived", source)
	require.NotNil(t, date, "a 30-day shelf life gives the derived batch a date")

	typedDate, typedSource := batchExpiry(t, ctx, typed)
	assert.Equal(t, "user", typedSource)
	require.NotNil(t, typedDate)
	assert.Equal(t, chosen.Format(time.DateOnly), typedDate.Format(time.DateOnly),
		"a date a person set outranks every rule, forever")
}

// TestUpdateProductShelfLifeNullResumesTheChain: null is not "no expiry", it is
// "resolve it the way spec 08 resolves everything else" — here, through the
// category rule the product is filed under.
func TestUpdateProductShelfLifeNullResumesTheChain(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	category, err := s.CreateCategory(ctx, f.storageID, store.NewCategory{
		Name: "Dry goods", DefaultShelfLifeDays: ptrInt(400),
	})
	require.NoError(t, err)

	_, _, err = s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		CategoryID: &category.ID, SetCategoryID: true,
		DefaultShelfLifeDays: ptrInt(2), SetDefaultShelfLifeDays: true,
	})
	require.NoError(t, err)

	batchID := f.stock(t, ctx, s, f.survivor.ID, 1)
	withOverride, _ := batchExpiry(t, ctx, batchID)
	require.NotNil(t, withOverride)

	updated, recomputed, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		SetDefaultShelfLifeDays: true,
	})
	require.NoError(t, err)
	assert.Nil(t, updated.DefaultShelfLifeDays, "null clears the override")
	assert.Equal(t, 1, recomputed)

	withCategoryRule, source := batchExpiry(t, ctx, batchID)
	assert.Equal(t, "derived", source)
	require.NotNil(t, withCategoryRule)
	assert.True(t, withCategoryRule.After(*withOverride),
		"clearing a 2-day override falls back to the category's 400 days")
}

// TestUpdateProductRejectsCategoryFromAnotherStorage: the same-storage rule a
// foreign key cannot express, on the PATCH route's category_id
// (docs/specs/03-auth-and-multi-tenancy.md).
func TestUpdateProductRejectsCategoryFromAnotherStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	theirs := newStorage(t, ctx)
	foreign, err := s.CreateCategory(ctx, theirs, store.NewCategory{Name: "Their Dairy"})
	require.NoError(t, err)

	_, _, err = s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		Name: ptrString("Renamed"), CategoryID: &foreign.ID, SetCategoryID: true,
	})
	require.ErrorIs(t, err, store.ErrNotFound)

	after, err := s.GetProduct(ctx, f.storageID, f.survivor.ID)
	require.NoError(t, err)
	assert.Equal(t, "Barilla Penne", after.Name, "the whole patch rolls back, name included")
	assert.Nil(t, after.CategoryID)
}

// TestUpdateProductLeavesUnmentionedFieldsAlone: a PATCH is a patch. Anything
// the body does not name writes itself back.
func TestUpdateProductLeavesUnmentionedFieldsAlone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	before, _, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		ItemType: itemTypePtr(store.ItemPerishable),
		MinStock: ptrInt(4),
		IconName: ptrString("noto:spaghetti"), SetIconName: true,
	})
	require.NoError(t, err)

	after, _, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		Name: ptrString("Penne Rigate"),
	})
	require.NoError(t, err)

	assert.Equal(t, "Penne Rigate", after.Name)
	assert.Equal(t, before.ItemType, after.ItemType)
	assert.Equal(t, before.MinStock, after.MinStock)
	require.NotNil(t, after.IconName)
	assert.Equal(t, "noto:spaghetti", *after.IconName)
	assert.True(t, after.UpdatedAt.After(before.UpdatedAt) || after.UpdatedAt.Equal(before.UpdatedAt))
}

// TestUpdateProductClearsIconWithExplicitNull: the Set* flag is what makes
// clearing expressible at all — without it, "no icon" and "leave the icon"
// would be the same request.
func TestUpdateProductClearsIconWithExplicitNull(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	_, _, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		IconName: ptrString("noto:cheese-wedge"), SetIconName: true,
	})
	require.NoError(t, err)

	cleared, _, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{SetIconName: true})
	require.NoError(t, err)
	assert.Nil(t, cleared.IconName)
}

// TestMergePreservesStockAndWritesNoLogs is the heart of the merge: a merge
// changes nothing about how much stock exists, only which product row it
// belongs to. Every log row of both survives under one product_id, and not one
// new row is written (docs/specs/16-product-maintenance.md).
func TestMergePreservesStockAndWritesNoLogs(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	f.stock(t, ctx, s, f.survivor.ID, 3)
	f.stock(t, ctx, s, f.source.ID, 5)
	f.stock(t, ctx, s, f.source.ID, 2)

	survivorStock, err := s.CurrentStock(ctx, f.storageID, f.survivor.ID)
	require.NoError(t, err)
	sourceStock, err := s.CurrentStock(ctx, f.storageID, f.source.ID)
	require.NoError(t, err)
	logsBefore := logCount(t, ctx, f.survivor.ID) + logCount(t, ctx, f.source.ID)
	require.Equal(t, 3, logsBefore, "three purchases, three log rows")

	result, err := s.MergeProducts(ctx, f.storageID, f.survivor.ID, f.source.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, result.MovedBatches)

	merged, err := s.CurrentStock(ctx, f.storageID, f.survivor.ID)
	require.NoError(t, err)
	assert.Equal(t, survivorStock+sourceStock, merged,
		"the survivor's stock is the sum of both prior stocks")

	assert.Equal(t, logsBefore, logCount(t, ctx, f.survivor.ID),
		"every log row of both is preserved, under one product_id, and none is invented")
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1`, f.source.ID))
}

// TestMergeRecomputesOnlyTheMovedBatches is the discriminating test for "only
// the MOVED batches": the survivor's own derived batch is given a deliberately
// wrong date first, so that an over-broad cascade would silently *correct* it
// and be caught doing so. A recompute is idempotent, so without the stale date
// an implementation that recomputed everything would pass.
func TestMergeRecomputesOnlyTheMovedBatches(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	_, _, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		DefaultShelfLifeDays: ptrInt(10), SetDefaultShelfLifeDays: true,
	})
	require.NoError(t, err)
	_, _, err = s.UpdateProduct(ctx, f.storageID, f.source.ID, store.ProductPatch{
		DefaultShelfLifeDays: ptrInt(900), SetDefaultShelfLifeDays: true,
	})
	require.NoError(t, err)

	survivorBatch := f.stock(t, ctx, s, f.survivor.ID, 1)
	movedBatch := f.stock(t, ctx, s, f.source.ID, 1)

	// A stale derived date, written behind the store's back: still 'derived',
	// so any cascade that reaches this row would rewrite it.
	stale := day(2040, time.January, 1)
	_, err = execTest(ctx,
		`UPDATE inventory_batches SET expiration_date = $1 WHERE id = $2`, stale, survivorBatch)
	require.NoError(t, err)

	movedBefore, _ := batchExpiry(t, ctx, movedBatch)
	require.NotNil(t, movedBefore, "the source's 900-day rule gave it a date")

	result, err := s.MergeProducts(ctx, f.storageID, f.survivor.ID, f.source.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, result.RecomputedBatches, "exactly the moved batch is recomputed")

	untouched, source := batchExpiry(t, ctx, survivorBatch)
	assert.Equal(t, "derived", source)
	require.NotNil(t, untouched)
	assert.Equal(t, stale.Format(time.DateOnly), untouched.Format(time.DateOnly),
		"the survivor's own batches are not part of the merge and must not be recomputed")

	movedAfter, movedSource := batchExpiry(t, ctx, movedBatch)
	assert.Equal(t, "derived", movedSource)
	require.NotNil(t, movedAfter)
	assert.True(t, movedAfter.Before(*movedBefore),
		"the moved batch now follows the survivor's shorter rule")
	assert.Equal(t, f.survivor.ID, batchProduct(t, ctx, movedBatch))
}

// TestMergeLeavesMovedUserDatesAlone: 'user' outranks the survivor's rules the
// same way it outranks every other rule.
func TestMergeLeavesMovedUserDatesAlone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	_, _, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		DefaultShelfLifeDays: ptrInt(1), SetDefaultShelfLifeDays: true,
	})
	require.NoError(t, err)

	typed := f.stock(t, ctx, s, f.source.ID, 1)
	chosen := day(2033, time.July, 4)
	_, err = s.SetBatchExpiration(ctx, f.storageID, typed, chosen, nil)
	require.NoError(t, err)

	result, err := s.MergeProducts(ctx, f.storageID, f.survivor.ID, f.source.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, result.RecomputedBatches, "a user date is not a derived one")

	date, source := batchExpiry(t, ctx, typed)
	assert.Equal(t, "user", source)
	require.NotNil(t, date)
	assert.Equal(t, chosen.Format(time.DateOnly), date.Format(time.DateOnly))
}

// TestMergeKeepsSurvivorFieldsAndTombstonesTheSource: the survivor's
// description wins unchanged, and the source leaves a tombstone so a
// delta-sync client drops it (docs/specs/12-client-api-contract.md).
func TestMergeKeepsSurvivorFieldsAndTombstonesTheSource(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	category, err := s.CreateCategory(ctx, f.storageID, store.NewCategory{Name: "Pasta"})
	require.NoError(t, err)
	before, _, err := s.UpdateProduct(ctx, f.storageID, f.survivor.ID, store.ProductPatch{
		CategoryID: &category.ID, SetCategoryID: true,
		ItemType: itemTypePtr(store.ItemLongShelfLife),
		MinStock: ptrInt(2),
		IconName: ptrString("noto:spaghetti"), SetIconName: true,
		DefaultShelfLifeDays: ptrInt(365), SetDefaultShelfLifeDays: true,
	})
	require.NoError(t, err)

	// Everything about the source differs, so any field-mixing would show.
	_, _, err = s.UpdateProduct(ctx, f.storageID, f.source.ID, store.ProductPatch{
		ItemType: itemTypePtr(store.ItemPerishable),
		MinStock: ptrInt(99),
		IconName: ptrString("noto:cheese-wedge"), SetIconName: true,
		DefaultShelfLifeDays: ptrInt(7), SetDefaultShelfLifeDays: true,
	})
	require.NoError(t, err)

	cursor := timeNow(t, ctx)
	result, err := s.MergeProducts(ctx, f.storageID, f.survivor.ID, f.source.ID)
	require.NoError(t, err)

	after := result.Survivor
	assert.Equal(t, before.Name, after.Name)
	assert.Equal(t, before.CategoryID, after.CategoryID)
	assert.Equal(t, before.ItemType, after.ItemType)
	assert.Equal(t, before.MinStock, after.MinStock)
	assert.Equal(t, before.IconName, after.IconName)
	assert.Equal(t, before.DefaultShelfLifeDays, after.DefaultShelfLifeDays)
	assert.Equal(t, before.ImageURL, after.ImageURL)
	assert.Equal(t, before.CreatedAt, after.CreatedAt)
	assert.True(t, after.UpdatedAt.After(before.UpdatedAt), "only updated_at moves")

	_, err = s.GetProduct(ctx, f.storageID, f.source.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the source id is gone from every read")

	delta, err := s.ProductsChangedSince(ctx, f.storageID, cursor)
	require.NoError(t, err)
	assert.Contains(t, delta.Deleted, f.source.ID,
		"a delta-sync client is told to drop the merged-away product")
}

// TestMergeRepointsShoppingListMatches: matched_product_id is ON DELETE SET
// NULL, so a merge that deleted before re-pointing would silently unresolve
// every line that pointed at the duplicate instead of failing loudly.
func TestMergeRepointsShoppingListMatches(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	_, items, err := s.CreateShoppingList(ctx, f.storageID, store.SourceText, nil,
		[]store.NewShoppingListItem{{RawText: "penne", Status: store.ItemExactMatch, MatchedProductID: &f.source.ID}})
	require.NoError(t, err)
	require.Len(t, items, 1)

	_, err = s.MergeProducts(ctx, f.storageID, f.survivor.ID, f.source.ID)
	require.NoError(t, err)

	var matched *uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT matched_product_id FROM shopping_list_items WHERE id = $1`, items[0].ID).Scan(&matched))
	require.NotNil(t, matched, "the match must survive the merge, not be nulled by the delete")
	assert.Equal(t, f.survivor.ID, *matched)
}

// TestMergeRejectsForeignAndSelf: both ids must be in the URL's storage, and a
// product cannot be merged into itself.
func TestMergeRejectsForeignAndSelf(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	theirs := newStorage(t, ctx)
	foreign, err := s.CreateProduct(ctx, theirs, store.NewProduct{Name: "Their Penne"})
	require.NoError(t, err)
	invented := newUUID(t)

	t.Run("source in another storage", func(t *testing.T) {
		_, err := s.MergeProducts(ctx, f.storageID, f.survivor.ID, foreign.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
	t.Run("survivor in another storage", func(t *testing.T) {
		_, err := s.MergeProducts(ctx, f.storageID, foreign.ID, f.survivor.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
	t.Run("nonexistent is the same answer as foreign", func(t *testing.T) {
		_, foreignErr := s.MergeProducts(ctx, f.storageID, f.survivor.ID, foreign.ID)
		_, missingErr := s.MergeProducts(ctx, f.storageID, f.survivor.ID, invented)
		require.Error(t, foreignErr)
		require.Error(t, missingErr)
		assert.Equal(t, missingErr.Error(), foreignErr.Error(),
			"a foreign id must not be distinguishable from a nonexistent one, even by message")
	})
	t.Run("self-merge", func(t *testing.T) {
		_, err := s.MergeProducts(ctx, f.storageID, f.survivor.ID, f.survivor.ID)
		assert.ErrorIs(t, err, store.ErrValidation)
	})

	// Nothing may have moved through any of the refusals.
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM products WHERE id = $1`, foreign.ID))
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM products WHERE id = $1`, f.source.ID))
}

// TestMergeAndDeleteLeaveTheCatalogUntouched: catalog_products is insert-only
// and describes what was once claimed, not the current state of any storage
// (docs/specs/02-data-model.md).
func TestMergeAndDeleteLeaveTheCatalogUntouched(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	entry, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{DisplayName: "Barilla Penne 500g"})
	require.NoError(t, err)

	survivor, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Penne", CatalogID: &entry.ID})
	require.NoError(t, err)
	source, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Penne 500g", CatalogID: &entry.ID})
	require.NoError(t, err)

	catalogBefore := countRows(t, ctx, `SELECT count(*) FROM catalog_products`)

	_, err = s.MergeProducts(ctx, storageID, survivor.ID, source.ID)
	require.NoError(t, err)
	assert.Equal(t, catalogBefore, countRows(t, ctx, `SELECT count(*) FROM catalog_products`))

	kept, err := s.GetProduct(ctx, storageID, survivor.ID)
	require.NoError(t, err)
	require.NotNil(t, kept.CatalogID)
	assert.Equal(t, entry.ID, *kept.CatalogID, "the survivor keeps its own catalog_id")

	_, err = s.DeleteProduct(ctx, storageID, survivor.ID)
	require.NoError(t, err)
	assert.Equal(t, catalogBefore, countRows(t, ctx, `SELECT count(*) FROM catalog_products`),
		"deleting a product never touches the row it may have seeded")
}

// TestDeleteProductReportsOrphanedImageOnlyWhenUnshared is the "unless shared"
// half of docs/specs/07-shopping-list-reconciliation.md's deletion rule: the
// file is the caller's to remove, and only when the last reference to it goes.
func TestDeleteProductReportsOrphanedImageOnlyWhenUnshared(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	shared := "/api/storages/" + storageID.String() + "/product-images/shared.jpg"
	lonely := "/api/storages/" + storageID.String() + "/product-images/lonely.jpg"

	first, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "A", ImageURL: &shared})
	require.NoError(t, err)
	second, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "B", ImageURL: &shared})
	require.NoError(t, err)
	alone, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "C", ImageURL: &lonely})
	require.NoError(t, err)

	orphaned, err := s.DeleteProduct(ctx, storageID, first.ID)
	require.NoError(t, err)
	assert.Empty(t, orphaned, "the file is still another product's picture")

	orphaned, err = s.DeleteProduct(ctx, storageID, second.ID)
	require.NoError(t, err)
	assert.Equal(t, shared, orphaned, "the last reference is gone, so the file may go")

	orphaned, err = s.DeleteProduct(ctx, storageID, alone.ID)
	require.NoError(t, err)
	assert.Equal(t, lonely, orphaned)
}

// TestMergeReportsOrphanedImageOnlyWhenUnshared is the same rule on the merge
// path: the source's picture is discarded with it unless something else uses
// the file — including, deliberately, the survivor.
func TestMergeReportsOrphanedImageOnlyWhenUnshared(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	picture := "/api/storages/" + storageID.String() + "/product-images/penne.jpg"

	t.Run("shared with the survivor", func(t *testing.T) {
		survivor, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "S1", ImageURL: &picture})
		require.NoError(t, err)
		source, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "D1", ImageURL: &picture})
		require.NoError(t, err)

		result, err := s.MergeProducts(ctx, storageID, survivor.ID, source.ID)
		require.NoError(t, err)
		assert.Empty(t, result.OrphanedImage)
	})

	t.Run("the source's own picture", func(t *testing.T) {
		own := "/api/storages/" + storageID.String() + "/product-images/own.jpg"
		survivor, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "S2"})
		require.NoError(t, err)
		source, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "D2", ImageURL: &own})
		require.NoError(t, err)

		result, err := s.MergeProducts(ctx, storageID, survivor.ID, source.ID)
		require.NoError(t, err)
		assert.Equal(t, own, result.OrphanedImage)
	})
}

// TestListProductLogsIsStorageScoped: a product of another storage answers the
// same ErrNotFound as one that does not exist, not an empty list — an empty
// list would confirm the id names a real row somewhere.
func TestListProductLogsIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	f.stock(t, ctx, s, f.survivor.ID, 2)
	theirs := newStorage(t, ctx)

	logs, err := s.ListProductLogs(ctx, f.storageID, f.survivor.ID, 50)
	require.NoError(t, err)
	assert.Len(t, logs, 1)
	assert.Equal(t, store.ReasonPurchase, logs[0].Reason)

	_, err = s.ListProductLogs(ctx, theirs, f.survivor.ID, 50)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.ListProductLogs(ctx, f.storageID, newUUID(t), 50)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestMergedHistoryIsTheUnionOfBoth: after a merge the survivor's ledger shows
// both products' rows, each keeping its own timestamp and reason, because the
// two names always were one product.
func TestMergedHistoryIsTheUnionOfBoth(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	f := newMergeFixture(t, ctx, s)
	f.stock(t, ctx, s, f.survivor.ID, 1)
	f.stock(t, ctx, s, f.source.ID, 4)

	_, err := s.MergeProducts(ctx, f.storageID, f.survivor.ID, f.source.ID)
	require.NoError(t, err)

	logs, err := s.ListProductLogs(ctx, f.storageID, f.survivor.ID, 50)
	require.NoError(t, err)
	require.Len(t, logs, 2)

	quantities := []int{logs[0].ChangeQty, logs[1].ChangeQty}
	assert.ElementsMatch(t, []int{1, 4}, quantities)
}

func itemTypePtr(t store.ItemType) *store.ItemType { return &t }
