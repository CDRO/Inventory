package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
