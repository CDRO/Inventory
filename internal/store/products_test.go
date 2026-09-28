package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/gamification"
	"github.com/CDRO/Inventory/internal/store"
)

// TestListProductsIsAlphabeticalAndStorageScoped — the manual-correction
// picker in docs/specs/09-consumption-logging.md needs the whole list, in a
// stable order, and never another storage's products.
func TestListProductsIsAlphabeticalAndStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	for _, name := range []string{"Rice", "Apples", "Milk"} {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: name})
		require.NoError(t, err)
	}
	_, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)

	products, err := s.ListProducts(ctx, storageID)
	require.NoError(t, err)
	require.Len(t, products, 3)

	names := make([]string, len(products))
	for i, p := range products {
		names[i] = p.Name
	}
	assert.Equal(t, []string{"Apples", "Milk", "Rice"}, names)
}

func TestListProductsIsEmptyNotNilForAnUnknownStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	products, err := s.ListProducts(ctx, newStorage(t, ctx))
	require.NoError(t, err)
	assert.NotNil(t, products)
	assert.Empty(t, products)
}

// TestSearchProductsFiltersByNameAndIsStorageScoped is the database half of
// products.html's list table (docs/specs/16-product-maintenance.md): an
// empty query is "Show all products", a non-empty one is a case-insensitive
// substring match, and another storage's products never leak in.
func TestSearchProductsFiltersByNameAndIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	for _, name := range []string{"Barilla Penne", "Oat Milk", "Whole Milk"} {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: name})
		require.NoError(t, err)
	}
	_, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Their Milk"})
	require.NoError(t, err)

	all, err := s.SearchProducts(ctx, storageID, "")
	require.NoError(t, err)
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = p.Name
	}
	assert.Equal(t, []string{"Barilla Penne", "Oat Milk", "Whole Milk"}, names, "empty query is every product, alphabetical")

	filtered, err := s.SearchProducts(ctx, storageID, "milk")
	require.NoError(t, err)
	require.Len(t, filtered, 2, "case-insensitive substring match, storage-scoped")
	filteredNames := []string{filtered[0].Name, filtered[1].Name}
	assert.ElementsMatch(t, []string{"Oat Milk", "Whole Milk"}, filteredNames)

	none, err := s.SearchProducts(ctx, storageID, "cheese")
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestSearchProductsComputesCategoryAndStockLive: the two fields the plain
// id-and-name list never carried, each computed the same way an existing
// summary query already does (category via a join, current_stock via a live
// sum — ReorderProducts's own rule, "the number must never be able to
// drift").
func TestSearchProductsComputesCategoryAndStockLive(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy"})
	require.NoError(t, err)

	categorized, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk", CategoryID: &category.ID})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: categorized.ID, LocationID: location.ID, Quantity: 4, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	uncategorized, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Bread"})
	require.NoError(t, err)

	rows, err := s.SearchProducts(ctx, storageID, "")
	require.NoError(t, err)

	byID := map[string]store.ProductSummary{}
	for _, r := range rows {
		byID[r.ID.String()] = r
	}

	milk := byID[categorized.ID.String()]
	require.NotNil(t, milk.CategoryName)
	assert.Equal(t, "Dairy", *milk.CategoryName)
	assert.Equal(t, 4, milk.CurrentStock)

	bread := byID[uncategorized.ID.String()]
	assert.Nil(t, bread.CategoryName)
	assert.Equal(t, 0, bread.CurrentStock, "no batches at all sums to zero")
}

// TestSetProductCategoryAsUserPaysOnlyForClosingTheGap mirrors
// UpdateProductMinStockAsUser's rule (docs/specs/51-gamification-scoring.md):
// the reward is for closing a gap, not for tuning a value that was already
// there.
func TestSetProductCategoryAsUserPaysOnlyForClosingTheGap(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	first, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Pantry"})
	require.NoError(t, err)
	second, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Fridge"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item"})
	require.NoError(t, err)

	require.NoError(t, s.SetProductCategoryAsUser(ctx, storageID, product.ID, &first.ID, userID))
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled),
		"filling in a previously-unset category must pay once")

	require.NoError(t, s.SetProductCategoryAsUser(ctx, storageID, product.ID, &second.ID, userID))
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled),
		"re-categorizing an already-categorized product must not pay again")

	require.NoError(t, s.SetProductCategoryAsUser(ctx, storageID, product.ID, nil, userID))
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled),
		"clearing a category must not pay")
}

// TestSetProductImageAsUserPaysOnlyForClosingTheGap is the same rule for the
// "imageless" quest's write path.
func TestSetProductImageAsUserPaysOnlyForClosingTheGap(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item"})
	require.NoError(t, err)

	icon := "box"
	require.NoError(t, s.SetProductImageAsUser(ctx, storageID, product.ID, nil, &icon, userID))
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled),
		"setting a picture on a previously-bare product must pay once")

	otherIcon := "jar"
	require.NoError(t, s.SetProductImageAsUser(ctx, storageID, product.ID, nil, &otherIcon, userID))
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled),
		"changing an already-set picture must not pay again")

	require.NoError(t, s.SetProductImageAsUser(ctx, storageID, product.ID, nil, nil, userID))
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled),
		"clearing a picture must not pay")
}
