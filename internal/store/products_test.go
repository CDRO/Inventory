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
