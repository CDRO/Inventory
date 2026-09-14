package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// TestAnalyticsTotalItemsSumsQuantityAcrossProducts is the store-level half
// of the "total items stored" metric in
// docs/specs/11-reporting-and-analytics.md: SUM(inventory_batches.quantity)
// across every product in the storage, live at query time.
func TestAnalyticsTotalItemsSumsQuantityAcrossProducts(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	milk, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: milk.ID, LocationID: location.ID, Quantity: 5, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	butter, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: butter.ID, LocationID: location.ID, Quantity: 2, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	total, err := s.AnalyticsTotalItems(ctx, storageID)
	require.NoError(t, err)
	assert.Equal(t, 7, total)
}

// TestAnalyticsTotalItemsIsStorageScoped — another storage's stock never
// contributes to this one's total.
func TestAnalyticsTotalItemsIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	loc, err := s.CreateLocation(ctx, other, store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, other, store.NewBatch{
		ProductID: product.ID, LocationID: loc.ID, Quantity: 9, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	total, err := s.AnalyticsTotalItems(ctx, storageID)
	require.NoError(t, err)
	assert.Zero(t, total, "an empty storage's total is zero, not another storage's stock")
}

// TestAnalyticsLocationDistributionResolvesAncestorPath is the acceptance
// criterion that location distribution carries the full resolved ancestor
// path, not just a leaf name — "Basement > Right Shelf", not "Right Shelf".
func TestAnalyticsLocationDistributionResolvesAncestorPath(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	basement, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Right Shelf", ParentID: &basement.ID})
	require.NoError(t, err)
	// A location with no batches must never appear — an empty shelf
	// contributes nothing to the chart.
	_, err = s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Empty Shelf"})
	require.NoError(t, err)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: shelf.ID, Quantity: 37, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	rows, err := s.AnalyticsLocationDistribution(ctx, storageID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the empty location must not appear")
	assert.Equal(t, shelf.ID, rows[0].LocationID)
	assert.Equal(t, "Basement > Right Shelf", rows[0].LocationName)
	assert.Equal(t, 37, rows[0].ItemCount)
}

// TestAnalyticsLocationDistributionSumsMultipleBatchesAtOneLocation — two
// batches of two different products at the same shelf sum into one row, not
// two.
func TestAnalyticsLocationDistributionSumsMultipleBatchesAtOneLocation(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf"})
	require.NoError(t, err)

	milk, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: milk.ID, LocationID: shelf.ID, Quantity: 3, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	butter, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Butter"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: butter.ID, LocationID: shelf.ID, Quantity: 4, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	rows, err := s.AnalyticsLocationDistribution(ctx, storageID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 7, rows[0].ItemCount)
}

// TestAnalyticsLocationDistributionIsStorageScoped — another storage's
// locations never leak into this one's distribution.
func TestAnalyticsLocationDistributionIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	loc, err := s.CreateLocation(ctx, other, store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, other, store.NewBatch{
		ProductID: product.ID, LocationID: loc.ID, Quantity: 9, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	rows, err := s.AnalyticsLocationDistribution(ctx, storageID)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestAnalyticsTurnoverReconcilesWithReorderDashboard is the acceptance
// criterion in docs/specs/11-reporting-and-analytics.md: a product moving
// from in-stock to out-of-stock in a period shows a corresponding consumed
// delta in that period's turnover data — because both read the same
// inventory_batches/inventory_logs tables, purchased and consumed are summed
// with opposite-sign SQL from the one ledger the reorder dashboard also
// reads.
func TestAnalyticsTurnoverReconcilesWithReorderDashboard(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk", MinStock: 1})
	require.NoError(t, err)
	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 5, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	// Consume the whole batch: the product goes from in-stock to
	// out-of-stock, which the reorder dashboard would now show as
	// out_of_stock.
	require.NoError(t, s.AdjustBatch(ctx, storageID, batch.ID, -5, store.ReasonConsumption, nil))

	reorderRows, err := s.ReorderProducts(ctx, storageID)
	require.NoError(t, err)
	require.Len(t, reorderRows, 1)
	assert.Zero(t, reorderRows[0].CurrentStock, "the product is now out of stock")

	turnover, err := s.AnalyticsTurnover(ctx, storageID, store.GranularityMonth)
	require.NoError(t, err)
	require.Len(t, turnover, 1, "both the purchase and the consumption land in the current month")
	assert.Equal(t, 5, turnover[0].Purchased)
	assert.Equal(t, 5, turnover[0].Consumed, "consumed is reported positive, not as the ledger's negative change_qty")
}

// TestAnalyticsTurnoverSumsVisionIngestionAsPurchased — vision_ingestion is
// counted alongside purchase on the positive side, per the spec's grouping
// rule.
func TestAnalyticsTurnoverSumsVisionIngestionAsPurchased(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)

	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 3, Reason: store.ReasonVisionIngestion,
	})
	require.NoError(t, err)

	turnover, err := s.AnalyticsTurnover(ctx, storageID, store.GranularityMonth)
	require.NoError(t, err)
	require.Len(t, turnover, 1)
	assert.Equal(t, 3, turnover[0].Purchased)
	assert.Zero(t, turnover[0].Consumed)
}

// TestAnalyticsTurnoverIsStorageScoped — another storage's ledger never
// contributes to this one's turnover.
func TestAnalyticsTurnoverIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	other := newStorage(t, ctx)

	loc, err := s.CreateLocation(ctx, other, store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, other, store.NewBatch{
		ProductID: product.ID, LocationID: loc.ID, Quantity: 9, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	turnover, err := s.AnalyticsTurnover(ctx, storageID, store.GranularityMonth)
	require.NoError(t, err)
	assert.Empty(t, turnover)
}

// TestAnalyticsTurnoverWeekGranularityFormatsAnISOWeekPeriod — the
// ?granularity=week path buckets and labels periods differently from month.
func TestAnalyticsTurnoverWeekGranularityFormatsAnISOWeekPeriod(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	turnover, err := s.AnalyticsTurnover(ctx, storageID, store.GranularityWeek)
	require.NoError(t, err)
	require.Len(t, turnover, 1)
	assert.Regexp(t, `^\d{4}-W\d{2}$`, turnover[0].Period)
	assert.Equal(t, 1, turnover[0].Purchased)
}
