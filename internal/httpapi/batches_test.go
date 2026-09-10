package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

func TestSplitValidatesTheRequest(t *testing.T) {
	t.Parallel()

	target := uuid.New().String()

	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"zero quantity", `{"quantity":0,"target_location_id":"` + target + `"}`, "quantity"},
		{"negative quantity", `{"quantity":-2,"target_location_id":"` + target + `"}`, "quantity"},
		{"missing target", `{"quantity":1}`, "target_location_id"},
		{"malformed target", `{"quantity":1,"target_location_id":"nope"}`, "target_location_id"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/inventory-batches/"+uuid.New().String()+"/split", tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)[tc.wantField])
		})
	}
}

// TestSplitLeavesTheUpperBoundToTheStore pins a deliberate division of labour.
// Only the store holds a lock on the batch row, so only the store knows the
// current quantity; a bound checked here would be read from an unlocked row and
// could be stale by the time the transaction ran.
func TestSplitLeavesTheUpperBoundToTheStore(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.batches.splitErr = store.ErrValidation

	rec := f.do(http.MethodPost, f.base()+"/inventory-batches/"+uuid.New().String()+"/split",
		`{"quantity":999,"target_location_id":"`+uuid.New().String()+`"}`)

	assert.Equal(t, 999, f.batches.lastQuantity, "the handler must not second-guess the bound")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "validation_failed", errorCode(t, rec))
}

// TestSplitRecordsTheActingUser matters because inventory_logs.created_by is
// what makes the ledger an audit trail rather than a list of anonymous
// quantity changes.
func TestSplitRecordsTheActingUser(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/inventory-batches/"+uuid.New().String()+"/split",
		`{"quantity":1,"target_location_id":"`+uuid.New().String()+`"}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.NotNil(t, f.batches.lastActingUser)
	assert.Equal(t, f.user.ID, *f.batches.lastActingUser)
}

func TestSplitAnswers201WithTheNewBatch(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	target := uuid.New()
	f.batches.split = &store.Batch{
		ID: uuid.New(), ProductID: uuid.New(), LocationID: target, Quantity: 1,
		ExpirationSource: store.ExpirationUser,
	}

	rec := f.do(http.MethodPost, f.base()+"/inventory-batches/"+uuid.New().String()+"/split",
		`{"quantity":1,"target_location_id":"`+target.String()+`"}`)

	require.Equal(t, http.StatusCreated, rec.Code)

	var body struct {
		LocationID       uuid.UUID `json:"location_id"`
		Quantity         int       `json:"quantity"`
		ExpirationSource string    `json:"expiration_source"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, target, body.LocationID)
	assert.Equal(t, 1, body.Quantity)
	assert.Equal(t, "user", body.ExpirationSource,
		"a reviewer has to be able to see that a person typed this date")
}

// TestBatchExpirationSerializesAsACalendarDay guards a bug that only shows up
// for users west of UTC: expiration_date is a DATE, and rendering it as an
// RFC 3339 instant attaches a midnight-UTC time that a browser in, say,
// America/New_York displays as the previous day.
func TestBatchExpirationSerializesAsACalendarDay(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	expires := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	f.batches.moved = &store.Batch{
		ID: uuid.New(), LocationID: uuid.New(), Quantity: 2, ExpirationDate: &expires,
	}

	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
		`{"location_id":"`+uuid.New().String()+`"}`)

	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		ExpirationDate string `json:"expiration_date"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "2026-03-01", body.ExpirationDate)
	assert.NotContains(t, body.ExpirationDate, "T",
		"a DATE is a calendar day; an instant would shift it a day west of UTC")
}

func TestPatchBatchRequiresALocationID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(), `{}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotEmpty(t, errorFields(t, rec)["location_id"],
		"a PATCH that changed nothing must not answer 200 as though it had")
}

func TestPatchBatchRejectsAMalformedLocationID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(), `{"location_id":"nope"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotEmpty(t, errorFields(t, rec)["location_id"])
}

// TestBatchRoutesPassTheValidatedStorage is the same guarantee the location
// handlers have: the id reaching the store is the one the middleware checked,
// never one parsed out of the URL by the handler.
func TestBatchRoutesPassTheValidatedStorage(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	batchID := uuid.New()
	target := uuid.New()

	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+batchID.String(),
		`{"location_id":"`+target.String()+`"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, f.storageID, f.batches.lastStorageID)
	assert.Equal(t, batchID, f.batches.lastBatchID)
	assert.Equal(t, target, f.batches.lastTargetID)
}

// TestForeignBatchOrTargetIs404 covers the acceptance criterion directly: an id
// from another storage, whether the batch or the move target, is a 404 that
// looks exactly like a nonexistent one.
func TestForeignBatchOrTargetIs404(t *testing.T) {
	t.Parallel()

	t.Run("split", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.batches.splitErr = store.ErrNotFound

		rec := f.do(http.MethodPost, f.base()+"/inventory-batches/"+uuid.New().String()+"/split",
			`{"quantity":1,"target_location_id":"`+uuid.New().String()+`"}`)

		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "not_found", errorCode(t, rec))
	})

	t.Run("move", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.batches.moveErr = store.ErrNotFound

		rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
			`{"location_id":"`+uuid.New().String()+`"}`)

		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "not_found", errorCode(t, rec))
	})
}
