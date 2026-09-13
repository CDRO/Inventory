package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// TestListProductBatchesOrdersNearestExpiryFirst is the default the batch
// picker in docs/specs/09-consumption-logging.md relies on: the first-out
// batch is whatever expires soonest, and a batch with no date at all is never
// steered towards.
func TestListProductBatchesOrdersNearestExpiryFirst(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	far, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		ExpirationDate: day(2027, time.December, 1), ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	noDate, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		ExpirationDate: nil, ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	near, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		ExpirationDate: day(2027, time.January, 1), ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	batches, err := s.ListProductBatches(ctx, storageID, product.ID)
	require.NoError(t, err)
	require.Len(t, batches, 3)
	assert.Equal(t, []string{near.ID.String(), far.ID.String(), noDate.ID.String()},
		[]string{batches[0].ID.String(), batches[1].ID.String(), batches[2].ID.String()},
		"nearest expiration first, undated last")
}

// TestListProductBatchesIsStorageScoped — a product from another storage is
// ErrNotFound, the same answer as a product that does not exist at all.
func TestListProductBatchesIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)

	_, err = s.ListProductBatches(ctx, storageID, product.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.ListProductBatches(ctx, storageID, newUUID(t))
	assert.ErrorIs(t, err, store.ErrNotFound, "a nonexistent product is the same refusal")
}
