package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// The whole-inventory read (docs/specs/33-inventory-overview-table.md): every
// batch of one storage, with product, category and location names already
// resolved, filterable by location/category subtree and paginated by the
// shared id-only cursor.

func TestListInventoryBatchesScopesToStorageAndResolvesNames(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	otherStorage := newStorage(t, ctx)

	basement, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Right Shelf", ParentID: &basement.ID})
	require.NoError(t, err)

	pasta, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Pasta"})
	require.NoError(t, err)

	penne, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Barilla Penne 500g", CategoryID: &pasta.ID, ImageURL: ptrString("https://example.com/penne.jpg"),
	})
	require.NoError(t, err)

	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: penne.ID, LocationID: shelf.ID, Quantity: 3, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	// A neighbour's storage, so a row from it never leaks into this one's
	// listing even though both databases share the same tables.
	otherLocation, err := s.CreateLocation(ctx, otherStorage, store.NewLocation{Name: "Their Shelf"})
	require.NoError(t, err)
	theirProduct, err := s.CreateProduct(ctx, otherStorage, store.NewProduct{Name: "Their Product"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, otherStorage, store.NewBatch{
		ProductID: theirProduct.ID, LocationID: otherLocation.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	rows, err := s.ListInventoryBatches(ctx, storageID, store.InventoryBatchFilter{}, nil, 50)
	require.NoError(t, err)

	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, batch.ID, row.ID)
	assert.Equal(t, "Barilla Penne 500g", row.ProductName)
	require.NotNil(t, row.ImageURL)
	assert.Equal(t, "https://example.com/penne.jpg", *row.ImageURL)
	require.NotNil(t, row.CategoryName)
	assert.Equal(t, "Pasta", *row.CategoryName)
	assert.Equal(t, []string{"Basement", "Right Shelf"}, row.LocationPath, "root to leaf")
	assert.Equal(t, 3, row.Quantity)
}

func TestListInventoryBatchesLocationFilterIncludesDescendantsAndValidatesStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	otherStorage := newStorage(t, ctx)

	basement, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Right Shelf", ParentID: &basement.ID})
	require.NoError(t, err)
	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)

	inBasement, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "In Basement"})
	require.NoError(t, err)
	inKitchen, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "In Kitchen"})
	require.NoError(t, err)

	// The batch sits on the descendant (Right Shelf), not on Basement itself —
	// this is exactly what the descendant-inclusive filter must still catch.
	descendantBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: inBasement.ID, LocationID: shelf.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: inKitchen.ID, LocationID: kitchen.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	rows, err := s.ListInventoryBatches(ctx, storageID, store.InventoryBatchFilter{LocationID: &basement.ID}, nil, 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, descendantBatch.ID, rows[0].ID)

	// A location from another storage and one that names nothing at all both
	// answer the identical ErrNotFound — the same non-enumeration rule every
	// other filter id in the system follows.
	foreignLocation, err := s.CreateLocation(ctx, otherStorage, store.NewLocation{Name: "Foreign"})
	require.NoError(t, err)
	_, err = s.ListInventoryBatches(ctx, storageID, store.InventoryBatchFilter{LocationID: &foreignLocation.ID}, nil, 50)
	assert.ErrorIs(t, err, store.ErrNotFound)

	unknown := newUUID(t)
	_, err = s.ListInventoryBatches(ctx, storageID, store.InventoryBatchFilter{LocationID: &unknown}, nil, 50)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestListInventoryBatchesCategoryFilterIncludesDescendantsAndValidatesStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	otherStorage := newStorage(t, ctx)

	pantry, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Pantry"})
	require.NoError(t, err)
	pasta, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Pasta", ParentID: &pantry.ID})
	require.NoError(t, err)
	cleaning, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Cleaning"})
	require.NoError(t, err)

	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf"})
	require.NoError(t, err)

	penne, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Penne", CategoryID: &pasta.ID})
	require.NoError(t, err)
	soap, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Soap", CategoryID: &cleaning.ID})
	require.NoError(t, err)

	descendantBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: penne.ID, LocationID: shelf.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: soap.ID, LocationID: shelf.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	rows, err := s.ListInventoryBatches(ctx, storageID, store.InventoryBatchFilter{CategoryID: &pantry.ID}, nil, 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, descendantBatch.ID, rows[0].ID)

	foreignCategory, err := s.CreateCategory(ctx, otherStorage, store.NewCategory{Name: "Foreign"})
	require.NoError(t, err)
	_, err = s.ListInventoryBatches(ctx, storageID, store.InventoryBatchFilter{CategoryID: &foreignCategory.ID}, nil, 50)
	assert.ErrorIs(t, err, store.ErrNotFound)

	unknown := newUUID(t)
	_, err = s.ListInventoryBatches(ctx, storageID, store.InventoryBatchFilter{CategoryID: &unknown}, nil, 50)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestListInventoryBatchesCursorWalkNeitherDuplicatesNorDropsARow — the same
// shared id-only cursor every other list endpoint uses
// (internal/httpapi/pagination.go): fetching one page at a time with the
// previous page's last id as `after` must, in the end, visit exactly the rows
// that exist, once each, in id order.
func TestListInventoryBatchesCursorWalkNeitherDuplicatesNorDropsARow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Widget"})
	require.NoError(t, err)

	const total = 5
	want := make([]uuid.UUID, 0, total)
	for i := 0; i < total; i++ {
		batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
			ProductID: product.ID, LocationID: shelf.ID, Quantity: 1, Reason: store.ReasonPurchase,
		})
		require.NoError(t, err)
		want = append(want, batch.ID)
	}

	var got []uuid.UUID
	var after *uuid.UUID
	for {
		page, err := s.ListInventoryBatches(ctx, storageID, store.InventoryBatchFilter{}, after, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			got = append(got, row.ID)
		}
		last := page[len(page)-1].ID
		after = &last
		if len(page) < 2 {
			break
		}
	}

	assert.Equal(t, want, got, "every row visited exactly once, in id order, across the walk")
}
