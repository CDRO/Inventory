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

// The rule under test: every write to inventory_batches.quantity is paired, in
// the same transaction, with an inventory_logs row explaining it
// (docs/specs/02-data-model.md).
//
// It fails silently in the worst way. Stock still looks right, so nothing
// surfaces until someone asks the ledger a question — analytics (spec 11) or
// the gamification scoring (spec 51), both of which read inventory_logs as the
// record of what happened.

// stocked returns a storage with one product holding `qty` at one location.
func stocked(t *testing.T, ctx context.Context, s *store.Store, qty int) (storageID, productID, locationID, batchID uuid.UUID) {
	t.Helper()

	storageID = newStorage(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Canned Tomatoes"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Cellar"})
	require.NoError(t, err)
	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: qty, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	return storageID, product.ID, location.ID, batch.ID
}

func logCount(t *testing.T, ctx context.Context, productID uuid.UUID) int {
	t.Helper()
	return countRows(t, ctx, `SELECT count(*) FROM inventory_logs WHERE product_id = $1`, productID)
}

func TestCreateBatchWritesPairedLog(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, productID, _, batchID := stocked(t, ctx, s, 6)

	assert.Equal(t, 1, logCount(t, ctx, productID))

	var changeQty int
	var reason string
	var loggedBatch *uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT change_qty, reason, batch_id FROM inventory_logs WHERE product_id = $1`,
		productID).Scan(&changeQty, &reason, &loggedBatch))

	assert.Equal(t, 6, changeQty, "the log records the quantity that arrived")
	assert.Equal(t, "purchase", reason)
	require.NotNil(t, loggedBatch)
	assert.Equal(t, batchID, *loggedBatch)
}

func TestAdjustBatchWritesPairedLog(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, _, batchID := stocked(t, ctx, s, 6)

	require.NoError(t, s.AdjustBatch(ctx, storageID, batchID, -2, store.ReasonConsumption, nil))

	var quantity int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT quantity FROM inventory_batches WHERE id = $1`, batchID).Scan(&quantity))
	assert.Equal(t, 4, quantity)

	assert.Equal(t, 2, logCount(t, ctx, productID), "the create and the adjust each logged once")
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1 AND change_qty = -2 AND reason = 'consumption'`,
		productID))
}

// TestBatchEmptiedIsDeletedAndStillLogged covers the pairing on the path where
// the batch row disappears. A log written before the delete, or skipped
// because the row is gone, would lose the consumption that emptied it.
func TestBatchEmptiedIsDeletedAndStillLogged(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, _, batchID := stocked(t, ctx, s, 3)

	require.NoError(t, s.AdjustBatch(ctx, storageID, batchID, -3, store.ReasonConsumption, nil))

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE id = $1`, batchID),
		"a batch at zero is deleted; batches are physical things in a place")

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1 AND change_qty = -3`, productID),
		"the consumption that emptied the batch must still be recorded")

	// The product survives at zero stock, feeding the reorder dashboard.
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM products WHERE id = $1`, productID))
	stock, err := s.CurrentStock(ctx, storageID, productID)
	require.NoError(t, err)
	assert.Equal(t, 0, stock)
}

// TestAdjustBelowZeroIsRejectedNotClamped — clamping would record a
// consumption that did not happen and leave the ledger disagreeing with the
// shelf.
func TestAdjustBelowZeroIsRejectedNotClamped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, _, batchID := stocked(t, ctx, s, 2)
	before := logCount(t, ctx, productID)

	err := s.AdjustBatch(ctx, storageID, batchID, -5, store.ReasonConsumption, nil)

	require.ErrorIs(t, err, store.ErrValidation)

	var quantity int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT quantity FROM inventory_batches WHERE id = $1`, batchID).Scan(&quantity))
	assert.Equal(t, 2, quantity, "the batch must be untouched, not clamped to zero")
	assert.Equal(t, before, logCount(t, ctx, productID), "a rejected adjustment writes no log")
}

// TestSplitPreservesExpiryAndTotal is the acceptance criterion from spec 06,
// enforced here at the model level: the jars are the same jars, so their
// expiry does not reset, and moving one across the house does not change how
// many exist.
func TestSplitPreservesExpiryAndTotal(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Pickles"})
	require.NoError(t, err)
	cellar, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Cellar"})
	require.NoError(t, err)
	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)

	expiry := day(2027, 3, 14)
	source, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID:        product.ID,
		LocationID:       cellar.ID,
		Quantity:         3,
		ExpirationDate:   expiry,
		ExpirationSource: store.ExpirationUser,
		Reason:           store.ReasonPurchase,
	})
	require.NoError(t, err)

	created, err := s.SplitBatch(ctx, storageID, source.ID, 1, kitchen.ID, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, created.Quantity)
	assert.Equal(t, kitchen.ID, created.LocationID)
	require.NotNil(t, created.ExpirationDate)
	assert.Equal(t, expiry.Format("2006-01-02"), created.ExpirationDate.Format("2006-01-02"),
		"both halves keep the original expiration date")
	assert.Equal(t, store.ExpirationUser, created.ExpirationSource,
		"a date a person typed stays a date a person typed, on both halves")

	total, err := s.CurrentStock(ctx, storageID, product.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, total, "a split moves stock, it does not create or destroy it")

	// Two move rows, summing to zero.
	var moveRows, moveSum int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(change_qty), 0) FROM inventory_logs
		  WHERE product_id = $1 AND reason = 'move'`, product.ID).Scan(&moveRows, &moveSum))
	assert.Equal(t, 2, moveRows)
	assert.Equal(t, 0, moveSum, "a move nets to zero so product totals are unaffected")
}

func TestSplitRejectsWholeBatchAndOverdraw(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Jars"})
	require.NoError(t, err)
	cellar, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Cellar"})
	require.NoError(t, err)
	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)
	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: cellar.ID, Quantity: 3, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	for _, qty := range []int{0, -1, 3, 4} {
		_, err := s.SplitBatch(ctx, storageID, batch.ID, qty, kitchen.ID, nil)
		require.ErrorIsf(t, err, store.ErrValidation, "split of %d must be rejected", qty)
	}

	var quantity int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT quantity FROM inventory_batches WHERE id = $1`, batch.ID).Scan(&quantity))
	assert.Equal(t, 3, quantity)
}

// TestDeleteLocationRefusesWhileDescendantHoldsStock — the check has to cover
// the whole subtree, because the location cascade would take the descendant
// out too and orphan stock that physically exists.
func TestDeleteLocationRefusesWhileDescendantHoldsStock(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	basement, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf", ParentID: &basement.ID})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: shelf.ID, Quantity: 4, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	err = s.DeleteLocation(ctx, storageID, basement.ID)

	require.ErrorIs(t, err, store.ErrConflict)
	assert.Equal(t, 2, countRows(t, ctx,
		`SELECT count(*) FROM locations WHERE storage_id = $1`, storageID),
		"neither the root nor the descendant may be removed")
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM tombstones WHERE storage_id = $1`, storageID),
		"a refused delete must not leave a tombstone behind")
}

// TestDeleteLocationWritesTombstonesForWholeSubtree — a client that cached the
// subtree has to learn the whole thing is gone, not only its root, or the
// descendants live in its cache forever.
func TestDeleteLocationWritesTombstonesForWholeSubtree(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	root, child, grandchild := locationChain(t, ctx, s, storageID)

	require.NoError(t, s.DeleteLocation(ctx, storageID, root))

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM locations WHERE storage_id = $1`, storageID))

	for _, id := range []uuid.UUID{root, child, grandchild} {
		assert.Equal(t, 1, countRows(t, ctx,
			`SELECT count(*) FROM tombstones WHERE storage_id = $1 AND entity_type = 'location' AND entity_id = $2`,
			storageID, id), "every removed node needs its own tombstone")
	}
}

func TestDeleteCategoryRefusesWhileProductUsesIt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	food, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	dairy, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy", ParentID: &food.ID})
	require.NoError(t, err)
	_, err = s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk", CategoryID: &dairy.ID})
	require.NoError(t, err)

	// Deleting the ancestor would cascade onto the category the product uses.
	err = s.DeleteCategory(ctx, storageID, food.ID)

	require.ErrorIs(t, err, store.ErrConflict)
	assert.Equal(t, 2, countRows(t, ctx,
		`SELECT count(*) FROM categories WHERE storage_id = $1`, storageID))
}

func TestDeleteProductWritesTombstoneInSameTransaction(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, _, _ := stocked(t, ctx, s, 2)

	require.NoError(t, s.DeleteProduct(ctx, storageID, productID))

	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM products WHERE id = $1`, productID))
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM tombstones WHERE entity_type = 'product' AND entity_id = $1`, productID))
}

// TestDeltaIsResumable covers the honesty rule for delta sync: a client whose
// cursor predates the oldest surviving tombstone cannot be brought up to date,
// and must be told to resync rather than handed an answer that quietly omits a
// swept deletion.
func TestDeltaIsResumable(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, _, _ := stocked(t, ctx, s, 1)

	fresh, err := s.DeltaIsResumable(ctx, storageID, timeNow(t, ctx))
	require.NoError(t, err)
	assert.True(t, fresh, "with no deletions recorded, any cursor is resumable")

	require.NoError(t, s.DeleteProduct(ctx, storageID, productID))

	tombstones, err := s.TombstonesSince(ctx, storageID, timeZero())
	require.NoError(t, err)
	require.Len(t, tombstones, 1)

	stale, err := s.DeltaIsResumable(ctx, storageID, timeZero())
	require.NoError(t, err)
	assert.False(t, stale, "a cursor older than the oldest tombstone cannot be resumed")
}

// TestMoveBatchWritesPairedMoveLogs holds the whole-batch move to the same rule
// the split obeys: two 'move' rows summing to zero, so product totals are
// untouched while the ledger still records that something happened.
func TestMoveBatchWritesPairedMoveLogs(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, cellar, batchID := stocked(t, ctx, s, 5)
	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)

	before := logCount(t, ctx, productID)

	moved, err := s.MoveBatch(ctx, storageID, batchID, kitchen.ID, nil)
	require.NoError(t, err)

	assert.Equal(t, batchID, moved.ID, "a move keeps the batch's identity; only a split makes a new row")
	assert.Equal(t, kitchen.ID, moved.LocationID)
	assert.NotEqual(t, cellar, moved.LocationID)
	assert.Equal(t, 5, moved.Quantity, "a move changes where stock is, never how much there is")

	assert.Equal(t, before+2, logCount(t, ctx, productID))

	var sum int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT coalesce(sum(change_qty), 0) FROM inventory_logs WHERE product_id = $1 AND reason = 'move'`,
		productID).Scan(&sum))
	assert.Zero(t, sum, "the pair must net to zero or product stock drifts on every move")

	assert.Equal(t, 2, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1 AND reason = 'move'`, productID))
}

// TestMoveBatchPreservesExpiry — the jars are the same jars. A move that reset
// the expiry, or downgraded a date a person typed back to a derived one, would
// let the cascade in docs/specs/08-expiration-and-classification.md overwrite
// a human decision later.
func TestMoveBatchPreservesExpiry(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, _, _ := stocked(t, ctx, s, 2)
	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)
	cellar, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Deep Cellar"})
	require.NoError(t, err)

	expires := time.Date(2027, 5, 4, 0, 0, 0, 0, time.UTC)
	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: cellar.ID, Quantity: 3,
		ExpirationDate: &expires, ExpirationSource: store.ExpirationUser,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	moved, err := s.MoveBatch(ctx, storageID, batch.ID, kitchen.ID, nil)
	require.NoError(t, err)

	require.NotNil(t, moved.ExpirationDate)
	assert.Equal(t, expires.Format(time.DateOnly), moved.ExpirationDate.Format(time.DateOnly))
	assert.Equal(t, store.ExpirationUser, moved.ExpirationSource,
		"a date a person typed stays a date a person typed")
}

// TestMoveBatchToTheSameLocationWritesNothing — a PATCH resending the value a
// batch already has must succeed without inventing history. Two 'move' rows
// that explain nothing would be indistinguishable, later, from a real move.
func TestMoveBatchToTheSameLocationWritesNothing(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID, productID, cellar, batchID := stocked(t, ctx, s, 5)
	before := logCount(t, ctx, productID)

	moved, err := s.MoveBatch(ctx, storageID, batchID, cellar, nil)
	require.NoError(t, err, "a no-op move is not an error; a client resending its own state must succeed")

	assert.Equal(t, cellar, moved.LocationID)
	assert.Equal(t, 5, moved.Quantity)
	assert.Equal(t, before, logCount(t, ctx, productID), "no move happened, so nothing is logged")
}
