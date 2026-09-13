package store_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// consumeFixture is a storage with one product holding two batches (nearer
// and farther expiration), a second unrelated product, and a done
// consumption job whose proposal has the given number of rows.
type consumeFixture struct {
	storageID  uuid.UUID
	userID     uuid.UUID
	beans      uuid.UUID
	batchNear  uuid.UUID
	batchFar   uuid.UUID
	otherBeans uuid.UUID
	jobID      uuid.UUID
}

func newConsumeFixture(t *testing.T, s *store.Store, rows int) consumeFixture {
	t.Helper()
	ctx := context.Background()

	f := consumeFixture{storageID: newStorage(t, ctx), userID: newUser(t, ctx)}

	pantry, err := s.CreateLocation(ctx, f.storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	beans, err := s.CreateProduct(ctx, f.storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)
	f.beans = beans.ID

	batchNear, err := s.CreateBatch(ctx, f.storageID, store.NewBatch{
		ProductID: beans.ID, LocationID: pantry.ID, Quantity: 3,
		ExpirationDate: day(2027, time.January, 1), ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	f.batchNear = batchNear.ID

	batchFar, err := s.CreateBatch(ctx, f.storageID, store.NewBatch{
		ProductID: beans.ID, LocationID: pantry.ID, Quantity: 5,
		ExpirationDate: day(2027, time.June, 1), ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	f.batchFar = batchFar.ID

	other, err := s.CreateProduct(ctx, f.storageID, store.NewProduct{Name: "Rice"})
	require.NoError(t, err)
	f.otherBeans = other.ID

	type row struct {
		RowID string `json:"row_id"`
	}
	proposal := struct {
		Rows []row `json:"rows"`
	}{Rows: []row{}}
	for i := range rows {
		proposal.Rows = append(proposal.Rows, row{RowID: strconv.Itoa(i)})
	}
	payload, err := json.Marshal(proposal)
	require.NoError(t, err)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: f.storageID, Kind: store.JobConsumptionPhoto})
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, job.ID, payload))
	f.jobID = job.ID

	return f
}

func TestConfirmConsumptionDecrementsBatchesInOneGo(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newConsumeFixture(t, s, 2)

	result, err := s.ConfirmConsumption(ctx, f.storageID, f.jobID, &f.userID, []store.ConsumeDecision{
		{RowID: "0", Accept: true, ProductID: &f.beans, Decrements: []store.ConsumeBatchDecrement{
			{BatchID: f.batchNear, Quantity: 2},
			{BatchID: f.batchFar, Quantity: 1},
		}},
		{RowID: "1", Accept: false},
	})
	require.NoError(t, err)
	assert.Len(t, result.BatchIDs, 2)

	assert.Equal(t, 1, countRows(t, ctx, `SELECT quantity FROM inventory_batches WHERE id = $1`, f.batchNear))
	assert.Equal(t, 4, countRows(t, ctx, `SELECT quantity FROM inventory_batches WHERE id = $1`, f.batchFar))

	assert.Equal(t, 2, countRows(t, ctx, `
		SELECT count(*) FROM inventory_logs l JOIN products p ON p.id = l.product_id
		 WHERE p.storage_id = $1 AND l.reason = 'consumption' AND l.created_by = $2 AND l.change_qty < 0`,
		f.storageID, f.userID), "every decrement is paired with a negative consumption log row")

	assert.Equal(t, "consumed", jobStatus(t, ctx, f.jobID))

	// A second confirm — a double submit, or another member reviewing at the
	// same time — is refused, not silently re-applied.
	_, err = s.ConfirmConsumption(ctx, f.storageID, f.jobID, &f.userID, []store.ConsumeDecision{
		{RowID: "0", Accept: false}, {RowID: "1", Accept: false},
	})
	assert.ErrorIs(t, err, store.ErrConflict)
	assert.Equal(t, 1, countRows(t, ctx, `SELECT quantity FROM inventory_batches WHERE id = $1`, f.batchNear))
}

// TestConfirmConsumptionDeletesAnEmptiedBatchButKeepsTheProduct is the
// acceptance criterion stated directly: a batch reaching zero disappears, a
// product at zero total stock does not.
func TestConfirmConsumptionDeletesAnEmptiedBatchButKeepsTheProduct(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newConsumeFixture(t, s, 1)

	_, err := s.ConfirmConsumption(ctx, f.storageID, f.jobID, &f.userID, []store.ConsumeDecision{
		{RowID: "0", Accept: true, ProductID: &f.beans, Decrements: []store.ConsumeBatchDecrement{
			{BatchID: f.batchNear, Quantity: 3},
			{BatchID: f.batchFar, Quantity: 5},
		}},
	})
	require.NoError(t, err)

	assert.Zero(t, countRows(t, ctx, `SELECT count(*) FROM inventory_batches WHERE id IN ($1, $2)`, f.batchNear, f.batchFar),
		"both emptied batches are gone")
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM products WHERE id = $1`, f.beans),
		"the product stays, browsable at zero stock (docs/specs/10-reorder-and-shopping-export.md)")

	stock, err := s.CurrentStock(ctx, f.storageID, f.beans)
	require.NoError(t, err)
	assert.Zero(t, stock)
}

// TestConfirmConsumptionRejectsADecrementBelowZero — the acceptance
// criterion: rejected, not clamped, and nothing about the row is written.
func TestConfirmConsumptionRejectsADecrementBelowZero(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newConsumeFixture(t, s, 1)

	_, err := s.ConfirmConsumption(ctx, f.storageID, f.jobID, &f.userID, []store.ConsumeDecision{
		{RowID: "0", Accept: true, ProductID: &f.beans, Decrements: []store.ConsumeBatchDecrement{
			{BatchID: f.batchNear, Quantity: 4}, // only 3 available
		}},
	})
	assert.ErrorIs(t, err, store.ErrValidation)
	assert.Equal(t, 3, countRows(t, ctx, `SELECT quantity FROM inventory_batches WHERE id = $1`, f.batchNear),
		"a refused decrement writes nothing")
	assert.Equal(t, "done", jobStatus(t, ctx, f.jobID))
}

// TestConfirmConsumptionRejectsABatchFromAnotherProduct — a decrement naming
// a real batch in this storage, but not the row's own product, must not
// quietly move stock off someone else's product.
func TestConfirmConsumptionRejectsABatchFromAnotherProduct(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newConsumeFixture(t, s, 1)

	_, err := s.ConfirmConsumption(ctx, f.storageID, f.jobID, &f.userID, []store.ConsumeDecision{
		{RowID: "0", Accept: true, ProductID: &f.otherBeans, Decrements: []store.ConsumeBatchDecrement{
			{BatchID: f.batchNear, Quantity: 1}, // belongs to f.beans, not f.otherBeans
		}},
	})
	assert.ErrorIs(t, err, store.ErrValidation)
	assert.Equal(t, 3, countRows(t, ctx, `SELECT quantity FROM inventory_batches WHERE id = $1`, f.batchNear))
}

func TestConfirmConsumptionRequiresExactlyTheIssuedRows(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newConsumeFixture(t, s, 2)

	accept := func(id string) store.ConsumeDecision {
		return store.ConsumeDecision{RowID: id, Accept: true, ProductID: &f.beans,
			Decrements: []store.ConsumeBatchDecrement{{BatchID: f.batchNear, Quantity: 1}}}
	}

	for name, decisions := range map[string][]store.ConsumeDecision{
		"missing row": {accept("0")},
		"unknown row": {accept("0"), accept("1"), accept("7")},
		"doubled row": {accept("0"), accept("0")},
		"none at all": {},
	} {
		_, err := s.ConfirmConsumption(ctx, f.storageID, f.jobID, &f.userID, decisions)
		assert.ErrorIs(t, err, store.ErrValidation, name)
	}

	assert.Equal(t, 3, countRows(t, ctx, `SELECT quantity FROM inventory_batches WHERE id = $1`, f.batchNear),
		"a refused confirm writes nothing")
	assert.Equal(t, "done", jobStatus(t, ctx, f.jobID))
}

func TestConfirmConsumptionRejectsAForeignBatchOrProduct(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newConsumeFixture(t, s, 1)
	other := newConsumeFixture(t, s, 0)

	for name, decision := range map[string]store.ConsumeDecision{
		"foreign product": {RowID: "0", Accept: true, ProductID: &other.beans,
			Decrements: []store.ConsumeBatchDecrement{{BatchID: other.batchNear, Quantity: 1}}},
		"foreign batch": {RowID: "0", Accept: true, ProductID: &f.beans,
			Decrements: []store.ConsumeBatchDecrement{{BatchID: other.batchNear, Quantity: 1}}},
	} {
		_, err := s.ConfirmConsumption(ctx, f.storageID, f.jobID, &f.userID, []store.ConsumeDecision{decision})
		assert.ErrorIs(t, err, store.ErrNotFound, name)
	}

	_, err := s.ConfirmConsumption(ctx, other.storageID, f.jobID, &f.userID, nil)
	assert.ErrorIs(t, err, store.ErrNotFound, "another storage cannot confirm this job")
}

func TestOnlyADoneConsumptionJobCanBeConfirmed(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	pending, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobConsumptionPhoto})
	require.NoError(t, err)
	_, err = s.ConfirmConsumption(ctx, storageID, pending.ID, nil, nil)
	assert.ErrorIs(t, err, store.ErrConflict)

	failed, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobConsumptionPhoto})
	require.NoError(t, err)
	require.NoError(t, s.FailJob(ctx, failed.ID, "nope"))
	_, err = s.ConfirmConsumption(ctx, storageID, failed.ID, nil, nil)
	assert.ErrorIs(t, err, store.ErrConflict)
}

func TestConfirmingAnEmptyConsumptionProposalOnlyConsumesIt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newConsumeFixture(t, s, 0)

	result, err := s.ConfirmConsumption(ctx, f.storageID, f.jobID, &f.userID, nil)
	require.NoError(t, err)
	assert.Empty(t, result.BatchIDs)
	assert.Equal(t, "consumed", jobStatus(t, ctx, f.jobID))
}
