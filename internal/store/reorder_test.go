package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// TestReorderProductsExcludesZeroThresholdAndComputesStockLive is the
// database-level half of the acceptance criteria in
// docs/specs/10-reorder-and-shopping-export.md: a product with min_stock = 0
// never appears in the result no matter how empty its shelf is, and
// current_stock is always the live sum of its batches, never a stored value.
func TestReorderProductsExcludesZeroThresholdAndComputesStockLive(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	untracked, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Salt", MinStock: 0})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: untracked.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	empty, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter", MinStock: 2})
	require.NoError(t, err)

	stocked, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk", MinStock: 3})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: stocked.ID, LocationID: location.ID, Quantity: 5, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	rows, err := s.ReorderProducts(ctx, storageID)
	require.NoError(t, err)

	byName := map[string]store.ReorderProduct{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	_, present := byName["Salt"]
	assert.False(t, present, "min_stock = 0 must never appear, regardless of actual stock")

	require.Contains(t, byName, "Butter")
	assert.Equal(t, empty.ID, byName["Butter"].ProductID)
	assert.Equal(t, 0, byName["Butter"].CurrentStock, "no batches at all sums to zero")

	require.Contains(t, byName, "Milk")
	assert.Equal(t, 5, byName["Milk"].CurrentStock, "current_stock is the live sum of batches")
}

// TestReorderProductsIsStorageScoped — another storage's tracked products
// never leak into this one's reorder list.
func TestReorderProductsIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	_, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Theirs", MinStock: 1})
	require.NoError(t, err)

	rows, err := s.ReorderProducts(ctx, storageID)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestUpdateProductMinStockIsStorageScoped mirrors the other product setters
// (SetProductCategory, DeleteProduct): a product in another storage is
// ErrNotFound, the same refusal a nonexistent one gets.
func TestUpdateProductMinStockIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)

	_, err = s.UpdateProductMinStock(ctx, storageID, product.ID, 3)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.UpdateProductMinStock(ctx, storageID, newUUID(t), 3)
	assert.ErrorIs(t, err, store.ErrNotFound, "a nonexistent product is the same refusal")
}

// TestUpdateProductMinStockAdjustsWithoutTouchingBatches is the store-level
// half of "adding a name that already exists adjusts min_stock instead of
// creating a second product": the call touches only products.min_stock, never
// creates a batch or a log row.
func TestUpdateProductMinStockAdjustsWithoutTouchingBatches(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter", MinStock: 0})
	require.NoError(t, err)

	updated, err := s.UpdateProductMinStock(ctx, storageID, product.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, updated.MinStock)
	assert.Equal(t, product.ID, updated.ID, "the same product, not a new row")

	batches, err := s.ListProductBatches(ctx, storageID, product.ID)
	require.NoError(t, err)
	assert.Empty(t, batches)
}
