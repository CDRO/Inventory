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

// The HTTP half of docs/specs/13-stocktake-and-audit.md. What these tests can
// see is the shape of the request a handler derived from a body — whether the
// exact row set matched, and whether the ledger row landed, is the store's and
// is covered in internal/store/stocktake_test.go.

// --- PATCH .../inventory-batches/{id} --------------------------------------

func TestPatchBatchPassesTheCountedQuantityThrough(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(), `{"quantity":4}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, f.batches.lastPatch.Quantity)
	assert.Equal(t, 4, *f.batches.lastPatch.Quantity)
	assert.Nil(t, f.batches.lastPatch.LocationID, "a count is not a move")
	require.NotNil(t, f.batches.lastActingUser)
	assert.Equal(t, f.user.ID, *f.batches.lastActingUser)
}

// TestPatchBatchToZeroAnswers204 — the store deletes an emptied batch, so
// there is no row left to return. A 200 carrying the batch would describe a
// row that no longer exists.
func TestPatchBatchToZeroAnswers204(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(), `{"quantity":0}`)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestPatchBatchCarriesAQuantityAndAMoveTogether(t *testing.T) {
	t.Parallel()

	target := uuid.New()
	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
		`{"quantity":2,"location_id":"`+target.String()+`"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, f.batches.lastPatch.Quantity)
	assert.Equal(t, 2, *f.batches.lastPatch.Quantity)
	require.NotNil(t, f.batches.lastPatch.LocationID)
	assert.Equal(t, target, *f.batches.lastPatch.LocationID,
		"both fields reach the store in one call, so they land in one transaction")
}

func TestPatchBatchValidatesTheRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"no field at all", `{}`, "quantity"},
		{"negative quantity", `{"quantity":-1}`, "quantity"},
		{"absurd quantity", `{"quantity":100001}`, "quantity"},
		{"fractional quantity", `{"quantity":2.5}`, "body"},
		{"malformed location", `{"location_id":"nope"}`, "location_id"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(), tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)[tc.wantField])
			assert.Nil(t, f.batches.lastPatch.Quantity, "a refused patch never reaches the store")
			assert.Nil(t, f.batches.lastPatch.LocationID)
		})
	}
}

// --- POST .../inventory-batches (found stock) ------------------------------

func foundStockBody(productID, locationID uuid.UUID, extra string) string {
	body := `{"product_id":"` + productID.String() + `","location_id":"` + locationID.String() + `","quantity":3`
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

// TestFoundStockIsRecordedAsAnAudit — the reason is the whole point of the
// route. 'purchase' would tell the turnover chart somebody went shopping.
func TestFoundStockIsRecordedAsAnAudit(t *testing.T) {
	t.Parallel()

	productID, locationID := uuid.New(), uuid.New()
	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/inventory-batches", foundStockBody(productID, locationID, ""))

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, store.ReasonAudit, f.stocktake.lastCreate.Reason)
	assert.Equal(t, productID, f.stocktake.lastCreate.ProductID)
	assert.Equal(t, locationID, f.stocktake.lastCreate.LocationID)
	assert.Equal(t, 3, f.stocktake.lastCreate.Quantity)
	require.NotNil(t, f.stocktake.lastCreate.CreatedBy)
	assert.Equal(t, f.user.ID, *f.stocktake.lastCreate.CreatedBy)
}

// TestFoundStockSeparatesAnOmittedDateFromAStatedOne is the distinction a
// plain *string would collapse. Absent means "resolve the shelf-life rules";
// an explicit null means "a person says this does not expire", which the
// expiry cascade must never overwrite
// (docs/specs/08-expiration-and-classification.md).
func TestFoundStockSeparatesAnOmittedDateFromAStatedOne(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		extra      string
		wantSource store.ExpirationSource
		wantDate   string
	}{
		{"omitted", "", store.ExpirationDerived, ""},
		{"explicit null", `"expiration_date":null`, store.ExpirationUser, ""},
		{"a date", `"expiration_date":"2027-01-10"`, store.ExpirationUser, "2027-01-10"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/inventory-batches",
				foundStockBody(uuid.New(), uuid.New(), tc.extra))

			require.Equal(t, http.StatusCreated, rec.Code)
			assert.Equal(t, tc.wantSource, f.stocktake.lastCreate.ExpirationSource)
			if tc.wantDate == "" {
				assert.Nil(t, f.stocktake.lastCreate.ExpirationDate)
				return
			}
			require.NotNil(t, f.stocktake.lastCreate.ExpirationDate)
			assert.Equal(t, tc.wantDate, f.stocktake.lastCreate.ExpirationDate.Format(time.DateOnly))
		})
	}
}

func TestFoundStockValidatesTheRequest(t *testing.T) {
	t.Parallel()

	id := uuid.New().String()
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"malformed product", `{"product_id":"nope","location_id":"` + id + `","quantity":1}`, "product_id"},
		{"malformed location", `{"product_id":"` + id + `","location_id":"nope","quantity":1}`, "location_id"},
		{"zero quantity", `{"product_id":"` + id + `","location_id":"` + id + `","quantity":0}`, "quantity"},
		{"malformed date", `{"product_id":"` + id + `","location_id":"` + id + `","quantity":1,"expiration_date":"10.01.2027"}`, "expiration_date"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/inventory-batches", tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)[tc.wantField])
			assert.Zero(t, f.stocktake.lastCreate.Quantity, "a refused body never reaches the store")
		})
	}
}

// TestFoundStockAnswers404ForAForeignId — the store drops the distinction
// between "not here" and "nowhere", and the handler must not reintroduce it.
func TestFoundStockAnswers404ForAForeignId(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.stocktake.createErr = store.ErrNotFound

	rec := f.do(http.MethodPost, f.base()+"/inventory-batches", foundStockBody(uuid.New(), uuid.New(), ""))

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// --- GET .../locations/{id}/stocktake --------------------------------------

func TestStocktakeSheetRendersWhatAWalkNeeds(t *testing.T) {
	t.Parallel()

	locationID, batchID, productID := uuid.New(), uuid.New(), uuid.New()
	expires := time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)
	audited := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	image := "/api/storages/x/product-images/penne.jpg"

	f := newAPIFixture(t)
	f.stocktake.sheet = &store.StocktakeSheet{
		Location: store.Location{ID: locationID, Name: "Layer 2", LastAuditedAt: &audited},
		Batches: []store.StocktakeBatch{{
			Batch: store.Batch{
				ID: batchID, ProductID: productID, LocationID: locationID, Quantity: 3,
				ExpirationDate: &expires, ExpirationSource: store.ExpirationDerived,
			},
			ProductName: "Barilla Penne 500g",
			ImageURL:    &image,
		}},
	}

	rec := f.do(http.MethodGet, f.base()+"/locations/"+locationID.String()+"/stocktake", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Location struct {
			ID            string  `json:"id"`
			Name          string  `json:"name"`
			LastAuditedAt *string `json:"last_audited_at"`
		} `json:"location"`
		Batches []struct {
			ID               string  `json:"id"`
			ProductName      string  `json:"product_name"`
			ImageURL         *string `json:"image_url"`
			Quantity         int     `json:"quantity"`
			ExpirationDate   *string `json:"expiration_date"`
			ExpirationSource string  `json:"expiration_source"`
		} `json:"batches"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, locationID.String(), body.Location.ID)
	assert.Equal(t, "Layer 2", body.Location.Name)
	require.NotNil(t, body.Location.LastAuditedAt)

	require.Len(t, body.Batches, 1)
	assert.Equal(t, batchID.String(), body.Batches[0].ID)
	assert.Equal(t, "Barilla Penne 500g", body.Batches[0].ProductName)
	require.NotNil(t, body.Batches[0].ImageURL)
	assert.Equal(t, 3, body.Batches[0].Quantity)
	require.NotNil(t, body.Batches[0].ExpirationDate)
	assert.Equal(t, "2027-01-10", *body.Batches[0].ExpirationDate,
		"a DATE is a calendar day, not an instant a client can render as the day before")
	assert.Equal(t, "derived", body.Batches[0].ExpirationSource)

	assert.Equal(t, locationID, f.stocktake.lastLocation)
	assert.Equal(t, f.storageID, f.stocktake.lastStorageID)
}

func TestStocktakeSheetAnswers404ForAMalformedOrForeignLocation(t *testing.T) {
	t.Parallel()

	t.Run("malformed", func(t *testing.T) {
		t.Parallel()
		f := newAPIFixture(t)
		rec := f.do(http.MethodGet, f.base()+"/locations/not-a-uuid/stocktake", "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "not_found", errorCode(t, rec))
	})

	t.Run("foreign", func(t *testing.T) {
		t.Parallel()
		f := newAPIFixture(t)
		f.stocktake.sheetErr = store.ErrNotFound
		rec := f.do(http.MethodGet, f.base()+"/locations/"+uuid.New().String()+"/stocktake", "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "not_found", errorCode(t, rec))
	})
}

// --- POST .../locations/{id}/stocktake -------------------------------------

func TestConfirmStocktakePassesEveryDecisionThrough(t *testing.T) {
	t.Parallel()

	locationID, first, second, foundProduct := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	f := newAPIFixture(t)
	f.stocktake.confirmed = &store.StocktakeResult{
		AuditedAt: time.Now(), Corrected: 1, CreatedBatchIDs: []uuid.UUID{uuid.New()},
	}

	rec := f.do(http.MethodPost, f.base()+"/locations/"+locationID.String()+"/stocktake", `{
		"batches": [{"batch_id":"`+first.String()+`","quantity":2},
		            {"batch_id":"`+second.String()+`","quantity":0}],
		"found":   [{"product_id":"`+foundProduct.String()+`","quantity":3,"expiration_date":null}]
	}`)

	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, f.stocktake.lastCounts, 2)
	assert.Equal(t, store.StocktakeCount{BatchID: first, Quantity: 2}, f.stocktake.lastCounts[0])
	assert.Equal(t, store.StocktakeCount{BatchID: second, Quantity: 0}, f.stocktake.lastCounts[1],
		"zero is a legitimate count — the shelf is empty of that one")

	require.Len(t, f.stocktake.lastFound, 1)
	assert.Equal(t, foundProduct, f.stocktake.lastFound[0].ProductID)
	assert.True(t, f.stocktake.lastFound[0].StatedExpiration, "an explicit null is a statement")
	assert.Nil(t, f.stocktake.lastFound[0].ExpirationDate)

	assert.Equal(t, locationID, f.stocktake.lastLocation)
	require.NotNil(t, f.stocktake.lastUser)
	assert.Equal(t, f.user.ID, *f.stocktake.lastUser)

	var body struct {
		LastAuditedAt   string   `json:"last_audited_at"`
		Corrected       int      `json:"corrected"`
		CreatedBatchIDs []string `json:"created_batch_ids"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 1, body.Corrected)
	assert.Len(t, body.CreatedBatchIDs, 1)
	assert.NotEmpty(t, body.LastAuditedAt)
}

// TestConfirmStocktakeRequiresABatchesArray — an absent key is a caller who
// has decided nothing, and the exact-row-set rule cannot read silence as a
// decision. An empty array is different: it is the correct statement for a
// shelf that holds nothing.
func TestConfirmStocktakeRequiresABatchesArray(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/locations/"+uuid.New().String()+"/stocktake", `{"found":[]}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotEmpty(t, errorFields(t, rec)["batches"])
	assert.Nil(t, f.stocktake.lastCounts, "a refused body never reaches the store")

	empty := newAPIFixture(t)
	ok := empty.do(http.MethodPost, empty.base()+"/locations/"+uuid.New().String()+"/stocktake", `{"batches":[]}`)
	assert.Equal(t, http.StatusOK, ok.Code, "an empty shelf is still a walk")
}

func TestConfirmStocktakeValidatesEveryEntry(t *testing.T) {
	t.Parallel()

	id := uuid.New().String()
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"malformed batch id", `{"batches":[{"batch_id":"nope","quantity":1}]}`, "batches[0].batch_id"},
		{"negative count", `{"batches":[{"batch_id":"` + id + `","quantity":-1}]}`, "batches[0].quantity"},
		{"malformed found product", `{"batches":[],"found":[{"product_id":"nope","quantity":1}]}`, "found[0].product_id"},
		{"zero found quantity", `{"batches":[],"found":[{"product_id":"` + id + `","quantity":0}]}`, "found[0].quantity"},
		{"malformed found date", `{"batches":[],"found":[{"product_id":"` + id + `","quantity":1,"expiration_date":"soon"}]}`, "found[0].expiration_date"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/locations/"+uuid.New().String()+"/stocktake", tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)[tc.wantField])
			assert.Nil(t, f.stocktake.lastCounts)
		})
	}
}

// TestConfirmStocktakeSurfacesAMismatchedShelfAs422 — the store refuses a
// sheet that no longer describes the shelf, and the wire answer has to tell
// the person what to do about it rather than leaking the store's own wording.
func TestConfirmStocktakeSurfacesAMismatchedShelfAs422(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.stocktake.confirmErr = store.ErrValidation

	rec := f.do(http.MethodPost, f.base()+"/locations/"+uuid.New().String()+"/stocktake",
		`{"batches":[{"batch_id":"`+uuid.New().String()+`","quantity":1}]}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "validation_failed", errorCode(t, rec))
	assert.NotEmpty(t, errorFields(t, rec)["batches"])
	assert.NotContains(t, rec.Body.String(), "store:", "the store's internal wording stays in the log")
}

func TestConfirmStocktakeAnswers404ForAForeignLocation(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.stocktake.confirmErr = store.ErrNotFound

	rec := f.do(http.MethodPost, f.base()+"/locations/"+uuid.New().String()+"/stocktake", `{"batches":[]}`)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestLocationTreeCarriesLastAuditedAt — locations.html renders staleness per
// node from this field, so its absence from the tree response would make the
// whole visibility feature silently invisible.
func TestLocationTreeCarriesLastAuditedAt(t *testing.T) {
	t.Parallel()

	audited := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	f := newAPIFixture(t)
	f.locations.tree = []store.Location{
		{ID: uuid.New(), Name: "Walked", LastAuditedAt: &audited},
		{ID: uuid.New(), Name: "Never walked"},
	}

	rec := f.do(http.MethodGet, f.base()+"/locations", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Items []struct {
			Name          string  `json:"name"`
			LastAuditedAt *string `json:"last_audited_at"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 2)
	require.NotNil(t, body.Items[0].LastAuditedAt)
	assert.Nil(t, body.Items[1].LastAuditedAt, "never audited is null, not a guessed date")
}
