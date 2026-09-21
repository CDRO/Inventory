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

// docs/specs/13-stocktake-and-audit.md is the first code path in the system
// that ever writes inventory_logs.reason = 'audit'. The CHECK has allowed the
// value since the first migration and nothing produced one, so every claim
// about audit rows — that a change writes exactly one, that an unchanged count
// writes none at all, that they never reach turnover or scoring — is tested
// here for the first time rather than inherited from an existing path.

// auditLogs returns the audit rows written for a product, newest last.
func auditLogs(t *testing.T, ctx context.Context, productID uuid.UUID) []struct {
	ChangeQty int
	BatchID   *uuid.UUID
} {
	t.Helper()

	rows, err := testPool.Query(ctx, `
		SELECT change_qty, batch_id
		  FROM inventory_logs
		 WHERE product_id = $1 AND reason = 'audit'
		 ORDER BY timestamp, id`, productID)
	require.NoError(t, err)
	defer rows.Close()

	var out []struct {
		ChangeQty int
		BatchID   *uuid.UUID
	}
	for rows.Next() {
		var row struct {
			ChangeQty int
			BatchID   *uuid.UUID
		}
		require.NoError(t, rows.Scan(&row.ChangeQty, &row.BatchID))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

func setQuantity(ctx context.Context, s *store.Store, storageID, batchID uuid.UUID, quantity int, userID *uuid.UUID) (*store.Batch, error) {
	return s.UpdateBatch(ctx, storageID, batchID, store.BatchPatch{Quantity: &quantity}, userID)
}

// TestCorrectingAQuantityWritesExactlyOneAuditRow is the first acceptance
// criterion, in both directions: the delta is signed, and it is one row, not
// one per unit and not one per field of the patch.
func TestCorrectingAQuantityWritesExactlyOneAuditRow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	t.Run("fewer than recorded", func(t *testing.T) {
		storageID, productID, _, batchID := stocked(t, ctx, s, 6)
		before := logCount(t, ctx, productID)

		updated, err := setQuantity(ctx, s, storageID, batchID, 4, nil)
		require.NoError(t, err)
		require.NotNil(t, updated)
		assert.Equal(t, 4, updated.Quantity)

		assert.Equal(t, before+1, logCount(t, ctx, productID), "exactly one row explains the correction")
		logs := auditLogs(t, ctx, productID)
		require.Len(t, logs, 1)
		assert.Equal(t, -2, logs[0].ChangeQty, "six counted as four is minus two, not four")
	})

	t.Run("more than recorded", func(t *testing.T) {
		storageID, productID, _, batchID := stocked(t, ctx, s, 2)

		_, err := setQuantity(ctx, s, storageID, batchID, 5, nil)
		require.NoError(t, err)

		logs := auditLogs(t, ctx, productID)
		require.Len(t, logs, 1)
		assert.Equal(t, 3, logs[0].ChangeQty)
	})
}

// TestAnUnchangedQuantityWritesNothing is the other half of the same criterion
// and the easier one to get wrong: a zero-delta row would satisfy "every batch
// write is paired with a log row" while filling the ledger with entries that
// explain nothing, and the analytics and scoring readers would have to learn
// to ignore them.
func TestAnUnchangedQuantityWritesNothing(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 3)

	before := logCount(t, ctx, productID)

	updated, err := setQuantity(ctx, s, storageID, batchID, 3, nil)
	require.NoError(t, err)
	require.NotNil(t, updated, "a no-op correction still answers with the batch")
	assert.Equal(t, 3, updated.Quantity)

	assert.Equal(t, before, logCount(t, ctx, productID), "there is no change to explain")
	assert.Empty(t, auditLogs(t, ctx, productID))
}

// TestCorrectingAQuantityToZeroDeletesTheBatch — batches do not linger at
// zero, and the row that explains the correction outlives the row it corrected.
func TestCorrectingAQuantityToZeroDeletesTheBatch(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 5)

	deleted, err := setQuantity(ctx, s, storageID, batchID, 0, nil)
	require.NoError(t, err)
	assert.Nil(t, deleted, "there is no batch left to answer with")

	assert.Zero(t, countRows(t, ctx, `SELECT count(*) FROM inventory_batches WHERE id = $1`, batchID))

	logs := auditLogs(t, ctx, productID)
	require.Len(t, logs, 1)
	assert.Equal(t, -5, logs[0].ChangeQty)
	assert.Nil(t, logs[0].BatchID, "batch_id is ON DELETE SET NULL, so the log keeps its meaning")
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM products WHERE id = $1`, productID),
		"emptying a batch never removes the product")
}

// TestCorrectingAQuantityRejectsANegativeCount — a shelf cannot hold minus one.
func TestCorrectingAQuantityRejectsANegativeCount(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 3)

	_, err := setQuantity(ctx, s, storageID, batchID, -1, nil)
	require.ErrorIs(t, err, store.ErrValidation)
	assert.Empty(t, auditLogs(t, ctx, productID))
}

// TestEmptyingAndMovingInOnePatchIsRefused — the two instructions contradict
// each other and applying them in either order gives a different answer.
func TestEmptyingAndMovingInOnePatchIsRefused(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 3)

	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)

	zero := 0
	_, err = s.UpdateBatch(ctx, storageID, batchID,
		store.BatchPatch{Quantity: &zero, LocationID: &kitchen.ID}, nil)
	require.ErrorIs(t, err, store.ErrValidation)

	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM inventory_batches WHERE id = $1`, batchID))
	assert.Empty(t, auditLogs(t, ctx, productID))
}

// TestCorrectingAQuantityAndMovingInOnePatchLandsTogether — spec 13 allows a
// PATCH to carry both, and each field keeps its own log semantics: one 'audit'
// row for the count, the pair of 'move' rows for the relocation.
func TestCorrectingAQuantityAndMovingInOnePatchLandsTogether(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 6)

	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)

	four := 4
	updated, err := s.UpdateBatch(ctx, storageID, batchID,
		store.BatchPatch{Quantity: &four, LocationID: &kitchen.ID}, nil)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, 4, updated.Quantity)
	assert.Equal(t, kitchen.ID, updated.LocationID)

	assert.Len(t, auditLogs(t, ctx, productID), 1, "the count change is one audit row")
	assert.Equal(t, 2, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1 AND reason = 'move'`, productID),
		"the relocation keeps its own paired move rows")
}

// TestFoundStockIsAuditedNotPurchased — the inverse correction. Turnover
// analytics count purchases, and a jar that was always on the shelf is not one.
func TestFoundStockIsAuditedNotPurchased(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	thirty := 30
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Penne", DefaultShelfLifeDays: &thirty})
	require.NoError(t, err)
	cellar, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Cellar"})
	require.NoError(t, err)

	created, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: cellar.ID, Quantity: 3, Reason: store.ReasonAudit,
	})
	require.NoError(t, err)

	assert.Zero(t, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1 AND reason = 'purchase'`, product.ID),
		"nothing was bought in this moment")

	logs := auditLogs(t, ctx, product.ID)
	require.Len(t, logs, 1)
	assert.Equal(t, 3, logs[0].ChangeQty)
	require.NotNil(t, logs[0].BatchID)
	assert.Equal(t, created.ID, *logs[0].BatchID)

	assert.Equal(t, store.ExpirationDerived, created.ExpirationSource)
	require.NotNil(t, created.ExpirationDate, "an omitted date resolves rather than staying unset")
	assert.Equal(t, time.Now().AddDate(0, 0, thirty).Format(time.DateOnly),
		created.ExpirationDate.Format(time.DateOnly))
}

// TestFoundStockWithAStatedNonExpiryStaysUser — an explicit "this does not
// expire" is a decision, and the cascade must never resolve over the top of
// it (docs/specs/08-expiration-and-classification.md).
func TestFoundStockWithAStatedNonExpiryStaysUser(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	thirty := 30
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Salt", DefaultShelfLifeDays: &thirty})
	require.NoError(t, err)
	cellar, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Cellar"})
	require.NoError(t, err)

	created, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: cellar.ID, Quantity: 1,
		ExpirationSource: store.ExpirationUser, Reason: store.ReasonAudit,
	})
	require.NoError(t, err)

	assert.Equal(t, store.ExpirationUser, created.ExpirationSource)
	assert.Nil(t, created.ExpirationDate, "a stated non-expiry is not an omission")
}

// TestAuditCorrectionsEarnNothing — spec 13's "Not scored". Paying points per
// correction would pay people to create errors to fix, and this is the first
// path that could ever have made that happen.
func TestAuditCorrectionsEarnNothing(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, batchID := stocked(t, ctx, s, 6)
	userID := newUser(t, ctx)

	_, err := setQuantity(ctx, s, storageID, batchID, 2, &userID)
	require.NoError(t, err)

	assert.Zero(t, countRows(t, ctx,
		`SELECT count(*) FROM user_progress WHERE storage_id = $1 AND user_id = $2`, storageID, userID),
		"an audit row scores nothing, so it does not even open a progress row")
}

// --- The guided walk -------------------------------------------------------

// shelf builds a location holding two batches of two different products.
func shelf(t *testing.T, ctx context.Context, s *store.Store) (storageID, locationID uuid.UUID, first, second *store.Batch) {
	t.Helper()

	storageID = newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Layer 2"})
	require.NoError(t, err)

	penne, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Penne"})
	require.NoError(t, err)
	passata, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Passata"})
	require.NoError(t, err)

	first, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: penne.ID, LocationID: location.ID, Quantity: 3, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	second, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: passata.ID, LocationID: location.ID, Quantity: 4, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	return storageID, location.ID, first, second
}

func lastAuditedAt(t *testing.T, ctx context.Context, locationID uuid.UUID) *time.Time {
	t.Helper()

	var at *time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT last_audited_at FROM locations WHERE id = $1`, locationID).Scan(&at))
	return at
}

// TestStocktakeSheetListsOnlyWhatIsOnThisShelf — a stocktake mirrors one
// physical shelf, so a child location's stock is its own walk, not part of
// this one.
func TestStocktakeSheetListsOnlyWhatIsOnThisShelf(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, locationID, first, second := shelf(t, ctx, s)

	child, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Front-Right", ParentID: &locationID})
	require.NoError(t, err)
	hidden, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Olives"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: hidden.ID, LocationID: child.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	sheet, err := s.LocationStocktake(ctx, storageID, locationID)
	require.NoError(t, err)

	assert.Equal(t, locationID, sheet.Location.ID)
	assert.Nil(t, sheet.Location.LastAuditedAt, "a never-walked shelf says so rather than guessing a date")

	ids := map[uuid.UUID]string{}
	for _, b := range sheet.Batches {
		ids[b.ID] = b.ProductName
	}
	require.Len(t, ids, 2)
	assert.Contains(t, ids, first.ID)
	assert.Contains(t, ids, second.ID)
	assert.Equal(t, "Penne", ids[first.ID], "the sheet names the product, so a person can recognise it")
}

// TestStocktakeSheetHidesAnotherStoragesLocation — 404-not-403, expressed in
// the store as the ErrNotFound a nonexistent id also gets.
func TestStocktakeSheetHidesAnotherStoragesLocation(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	_, locationID, _, _ := shelf(t, ctx, s)
	other := newStorage(t, ctx)

	_, err := s.LocationStocktake(ctx, other, locationID)
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.LocationStocktake(ctx, other, uuid.New())
	require.ErrorIs(t, err, store.ErrNotFound, "and a nonexistent one is answered identically")
}

// TestConfirmStocktakeAppliesEveryDecision — the ordinary case: one count
// down, one count up, one found item, each explained by exactly one audit row.
func TestConfirmStocktakeAppliesEveryDecision(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, locationID, first, second := shelf(t, ctx, s)

	found, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Chickpeas"})
	require.NoError(t, err)

	result, err := s.ConfirmStocktake(ctx, storageID, locationID, nil,
		[]store.StocktakeCount{
			{BatchID: first.ID, Quantity: 2},
			{BatchID: second.ID, Quantity: 6},
		},
		[]store.FoundStock{{ProductID: found.ID, Quantity: 3}})
	require.NoError(t, err)

	assert.Equal(t, 2, result.Corrected)
	require.Len(t, result.CreatedBatchIDs, 1)
	assert.False(t, result.AuditedAt.IsZero())

	assert.Equal(t, -1, auditLogs(t, ctx, first.ProductID)[0].ChangeQty)
	assert.Equal(t, 2, auditLogs(t, ctx, second.ProductID)[0].ChangeQty)
	assert.Equal(t, 3, auditLogs(t, ctx, found.ID)[0].ChangeQty)

	assert.Equal(t, 3, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE location_id = $1`, locationID),
		"the found batch joins the shelf it was found on")
}

// TestConfirmStocktakeSetsLastAuditedAtWhenNothingChanged — "I checked and it
// was right" is precisely the fact the timestamp records, so a walk that
// corrects nothing still has to leave one.
func TestConfirmStocktakeSetsLastAuditedAtWhenNothingChanged(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, locationID, first, second := shelf(t, ctx, s)

	before := logCount(t, ctx, first.ProductID) + logCount(t, ctx, second.ProductID)

	result, err := s.ConfirmStocktake(ctx, storageID, locationID, nil,
		[]store.StocktakeCount{
			{BatchID: first.ID, Quantity: first.Quantity},
			{BatchID: second.ID, Quantity: second.Quantity},
		}, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Corrected)

	assert.Equal(t, before, logCount(t, ctx, first.ProductID)+logCount(t, ctx, second.ProductID),
		"an unchanged shelf explains nothing")
	require.NotNil(t, lastAuditedAt(t, ctx, locationID), "but it was still walked")
}

// TestConfirmStocktakeOnAnEmptyShelfIsStillAWalk — an empty location has no
// counts to state, and stating none of them is the exact row set.
func TestConfirmStocktakeOnAnEmptyShelfIsStillAWalk(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Empty shelf"})
	require.NoError(t, err)

	_, err = s.ConfirmStocktake(ctx, storageID, location.ID, nil, nil, nil)
	require.NoError(t, err)
	assert.NotNil(t, lastAuditedAt(t, ctx, location.ID))
}

// TestConfirmStocktakeRejectsAnInexactRowSet is the exact-row-set rule spec 09
// established, applied here: a sheet that no longer describes the shelf is
// refused whole rather than partially applied. Every case asserts that nothing
// at all was written, because a refusal that had already corrected the first
// two batches would be the silent partial apply the rule exists to prevent.
func TestConfirmStocktakeRejectsAnInexactRowSet(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	cases := map[string]func(first, second *store.Batch) []store.StocktakeCount{
		"a batch left uncounted": func(first, _ *store.Batch) []store.StocktakeCount {
			return []store.StocktakeCount{{BatchID: first.ID, Quantity: 1}}
		},
		"a batch that is not on this shelf": func(first, second *store.Batch) []store.StocktakeCount {
			return []store.StocktakeCount{
				{BatchID: first.ID, Quantity: 1},
				{BatchID: second.ID, Quantity: 1},
				{BatchID: uuid.New(), Quantity: 1},
			}
		},
		"the same batch counted twice": func(first, second *store.Batch) []store.StocktakeCount {
			return []store.StocktakeCount{
				{BatchID: first.ID, Quantity: 1},
				{BatchID: first.ID, Quantity: 2},
				{BatchID: second.ID, Quantity: 1},
			}
		},
	}

	for name, counts := range cases {
		t.Run(name, func(t *testing.T) {
			storageID, locationID, first, second := shelf(t, ctx, s)
			found, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Chickpeas"})
			require.NoError(t, err)

			_, err = s.ConfirmStocktake(ctx, storageID, locationID, nil, counts(first, second),
				[]store.FoundStock{{ProductID: found.ID, Quantity: 3}})
			require.ErrorIs(t, err, store.ErrValidation)

			assert.Empty(t, auditLogs(t, ctx, first.ProductID))
			assert.Empty(t, auditLogs(t, ctx, second.ProductID))
			assert.Zero(t, countRows(t, ctx,
				`SELECT count(*) FROM inventory_batches WHERE product_id = $1`, found.ID),
				"the found stock is rolled back with everything else")
			assert.Equal(t, 2, countRows(t, ctx,
				`SELECT count(*) FROM inventory_batches WHERE location_id = $1`, locationID))
			assert.Nil(t, lastAuditedAt(t, ctx, locationID), "a refused walk is not a walk")
		})
	}
}

// TestConfirmStocktakeCountingToZeroDeletesTheBatch — the same rule the
// single-batch correction follows, reached through the walk.
func TestConfirmStocktakeCountingToZeroDeletesTheBatch(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, locationID, first, second := shelf(t, ctx, s)

	_, err := s.ConfirmStocktake(ctx, storageID, locationID, nil,
		[]store.StocktakeCount{
			{BatchID: first.ID, Quantity: 0},
			{BatchID: second.ID, Quantity: second.Quantity},
		}, nil)
	require.NoError(t, err)

	assert.Zero(t, countRows(t, ctx, `SELECT count(*) FROM inventory_batches WHERE id = $1`, first.ID))
	logs := auditLogs(t, ctx, first.ProductID)
	require.Len(t, logs, 1)
	assert.Equal(t, -3, logs[0].ChangeQty)
	assert.Nil(t, logs[0].BatchID)
}

// TestConfirmStocktakeRejectsForeignIds — every id in the payload is resolved
// against the URL's storage, and a foreign one is the same ErrNotFound a
// nonexistent one gets.
func TestConfirmStocktakeRejectsForeignIds(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	t.Run("the location", func(t *testing.T) {
		_, locationID, _, _ := shelf(t, ctx, s)
		other := newStorage(t, ctx)

		_, err := s.ConfirmStocktake(ctx, other, locationID, nil, nil, nil)
		require.ErrorIs(t, err, store.ErrNotFound)
		assert.Nil(t, lastAuditedAt(t, ctx, locationID))
	})

	t.Run("a found product", func(t *testing.T) {
		storageID, locationID, first, second := shelf(t, ctx, s)
		other := newStorage(t, ctx)
		theirs, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Not yours"})
		require.NoError(t, err)

		_, err = s.ConfirmStocktake(ctx, storageID, locationID, nil,
			[]store.StocktakeCount{
				{BatchID: first.ID, Quantity: first.Quantity},
				{BatchID: second.ID, Quantity: second.Quantity},
			},
			[]store.FoundStock{{ProductID: theirs.ID, Quantity: 1}})
		require.ErrorIs(t, err, store.ErrNotFound)

		assert.Nil(t, lastAuditedAt(t, ctx, locationID), "the whole walk rolls back")
		assert.Zero(t, countRows(t, ctx,
			`SELECT count(*) FROM inventory_batches WHERE product_id = $1`, theirs.ID))
	})

	t.Run("a batch on another storage's shelf", func(t *testing.T) {
		storageID, locationID, first, second := shelf(t, ctx, s)
		_, _, theirFirst, _ := shelf(t, ctx, s)

		// Deliberately ErrValidation rather than ErrNotFound: the id is being
		// compared against this shelf's set, not resolved on its own, and a
		// foreign id, an unknown id and an id on the wrong shelf all produce
		// the identical refusal, so there is nothing to tell apart.
		_, err := s.ConfirmStocktake(ctx, storageID, locationID, nil,
			[]store.StocktakeCount{
				{BatchID: first.ID, Quantity: first.Quantity},
				{BatchID: second.ID, Quantity: second.Quantity},
				{BatchID: theirFirst.ID, Quantity: 1},
			}, nil)
		require.ErrorIs(t, err, store.ErrValidation)
		assert.Nil(t, lastAuditedAt(t, ctx, locationID))
	})
}

// TestConfirmStocktakeRecordsTheActingUser — inventory_logs.created_by is what
// makes the ledger an audit trail rather than a list of anonymous corrections.
func TestConfirmStocktakeRecordsTheActingUser(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, locationID, first, second := shelf(t, ctx, s)
	userID := newUser(t, ctx)

	_, err := s.ConfirmStocktake(ctx, storageID, locationID, &userID,
		[]store.StocktakeCount{
			{BatchID: first.ID, Quantity: 1},
			{BatchID: second.ID, Quantity: second.Quantity},
		}, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE product_id = $1 AND reason = 'audit' AND created_by = $2`,
		first.ProductID, userID))
}
