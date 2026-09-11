package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// fakeExpiry records what the handlers asked the store to do, so a test can
// assert on the distinction the whole feature turns on: whether a write was
// recorded as a person's decision or as a derived value.
type fakeExpiry struct {
	setResult    *store.Batch
	setErr       error
	resetResult  *store.Batch
	resetErr     error
	shelfErr     error
	recomputed   int
	recomputeErr error

	setCalls      int
	resetCalls    int
	lastDate      *time.Time
	lastDateGiven bool
	lastBatchID   uuid.UUID
	lastCategory  uuid.UUID
	lastDays      *int
	lastDaysGiven bool
	lastStorageID uuid.UUID
}

func (f *fakeExpiry) SetBatchExpiration(_ context.Context, storageID, batchID uuid.UUID, date *time.Time) (*store.Batch, error) {
	f.setCalls++
	f.lastStorageID, f.lastBatchID = storageID, batchID
	f.lastDate, f.lastDateGiven = date, true
	if f.setErr != nil {
		return nil, f.setErr
	}
	if f.setResult != nil {
		return f.setResult, nil
	}
	return &store.Batch{ID: batchID, ExpirationDate: date, ExpirationSource: store.ExpirationUser}, nil
}

func (f *fakeExpiry) ResetBatchExpirationToDerived(_ context.Context, storageID, batchID uuid.UUID) (*store.Batch, error) {
	f.resetCalls++
	f.lastStorageID, f.lastBatchID = storageID, batchID
	if f.resetErr != nil {
		return nil, f.resetErr
	}
	if f.resetResult != nil {
		return f.resetResult, nil
	}
	return &store.Batch{ID: batchID, ExpirationSource: store.ExpirationDerived}, nil
}

func (f *fakeExpiry) SetCategoryShelfLife(_ context.Context, storageID, id uuid.UUID, days *int) error {
	f.lastStorageID, f.lastCategory = storageID, id
	f.lastDays, f.lastDaysGiven = days, true
	return f.shelfErr
}

func (f *fakeExpiry) RecomputeDerivedExpiryForCategory(_ context.Context, _, categoryID uuid.UUID) (int, error) {
	f.lastCategory = categoryID
	return f.recomputed, f.recomputeErr
}

func batchPath(f *apiFixture, id uuid.UUID) string {
	return f.base() + "/inventory-batches/" + id.String() + "/expiry"
}

// TestSettingADateRecordsItAsAUserDecision — the source is what protects the
// date from every later cascade, so the write has to record it.
func TestSettingADateRecordsItAsAUserDecision(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	id := uuid.New()

	rec := f.do(http.MethodPatch, batchPath(f, id), `{"expiration_date":"2026-03-01"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, f.expiry.setCalls)
	require.NotNil(t, f.expiry.lastDate)
	assert.Equal(t, "2026-03-01", f.expiry.lastDate.Format(time.DateOnly))

	var body struct {
		ExpirationSource string `json:"expiration_source"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "user", body.ExpirationSource)
}

// TestClearingADateIsAFirstClassState is the case with the worst failure mode:
// an explicit null must reach the store as a null, recorded as a user
// decision, not be mistaken for "no field sent".
func TestClearingADateIsAFirstClassState(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodPatch, batchPath(f, uuid.New()), `{"expiration_date":null}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, f.expiry.setCalls, "an explicit null is a write, not a no-op")
	assert.True(t, f.expiry.lastDateGiven)
	assert.Nil(t, f.expiry.lastDate, `null means "this has no expiry", and must arrive as null`)
}

// TestAnEmptyPatchIsRefused — omitting the field entirely is not the same as
// sending null, and answering 200 would tell the caller something landed.
func TestAnEmptyPatchIsRefused(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodPatch, batchPath(f, uuid.New()), `{}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotEmpty(t, errorFields(t, rec)["expiration_date"])
	assert.Zero(t, f.expiry.setCalls)
	assert.Zero(t, f.expiry.resetCalls)
}

// TestResetPutsABatchBackUnderTheRules — required rather than optional,
// because without it one accidental edit opts a batch out permanently.
func TestResetPutsABatchBackUnderTheRules(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodPatch, batchPath(f, uuid.New()), `{"expiration_source":"derived"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, f.expiry.resetCalls)
	assert.Zero(t, f.expiry.setCalls, "a reset is not a date write")

	var body struct {
		ExpirationSource string `json:"expiration_source"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "derived", body.ExpirationSource)
}

// TestAskingToBecomeUserSourcedIsRefused — 'user' is something the server
// records when a person sets a date, not a flag a client may assert. Allowing
// it would let a caller freeze a batch against every future rule change
// without ever choosing a date.
func TestAskingToBecomeUserSourcedIsRefused(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodPatch, batchPath(f, uuid.New()), `{"expiration_source":"user"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotEmpty(t, errorFields(t, rec)["expiration_source"])
	assert.Zero(t, f.expiry.setCalls)
	assert.Zero(t, f.expiry.resetCalls)
}

// TestOnlyACalendarDayIsAccepted — a DATE column is a day, not an instant.
// Accepting a timestamp would let a client's timezone decide which day a jar
// expires on.
func TestOnlyACalendarDayIsAccepted(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"expiration_date":"2026-03-01T12:00:00Z"}`,
		`{"expiration_date":"01/03/2026"}`,
		`{"expiration_date":"tomorrow"}`,
		`{"expiration_date":12345}`,
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPatch, batchPath(f, uuid.New()), body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)["expiration_date"])
			assert.Zero(t, f.expiry.setCalls)
		})
	}
}

func TestBatchExpiryIsStorageScoped(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.expiry.setErr = store.ErrNotFound

	rec := f.do(http.MethodPatch, batchPath(f, uuid.New()), `{"expiration_date":"2026-03-01"}`)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestChangingAShelfLifeRuleCascadesAndReportsTheCount — a rule change is a
// correction, so it applies to data already in the database. Returning the
// count is what lets the UI say what actually happened.
func TestChangingAShelfLifeRuleCascadesAndReportsTheCount(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.expiry.recomputed = 7
	categoryID := uuid.New()

	rec := f.do(http.MethodPatch,
		f.base()+"/categories/"+categoryID.String()+"/shelf-life",
		`{"default_shelf_life_days":14}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, f.expiry.lastDays)
	assert.Equal(t, 14, *f.expiry.lastDays)
	assert.Equal(t, categoryID, f.expiry.lastCategory)

	var body struct {
		RecomputedBatches int `json:"recomputed_batches"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 7, body.RecomputedBatches)
}

// TestANullShelfLifeMeansInherit — distinct from zero, which would mean
// "expires the day it arrives".
func TestANullShelfLifeMeansInherit(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodPatch,
		f.base()+"/categories/"+uuid.New().String()+"/shelf-life",
		`{"default_shelf_life_days":null}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, f.expiry.lastDaysGiven)
	assert.Nil(t, f.expiry.lastDays)
}

func TestShelfLifeValidatesItsInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"negative", `{"default_shelf_life_days":-1}`},
		{"absurdly long", `{"default_shelf_life_days":99999999}`},
		{"not a number", `{"default_shelf_life_days":"ten"}`},
		{"field omitted", `{}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPatch,
				f.base()+"/categories/"+uuid.New().String()+"/shelf-life", tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)["default_shelf_life_days"])
		})
	}
}

// TestExpiryRoutesAreBehindTheGateChain — same as every other storage-scoped
// route; adding one must not be a way around the gates.
func TestExpiryRoutesAreBehindTheGateChain(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	id := uuid.New().String()

	for _, path := range []string{
		f.base() + "/inventory-batches/" + id + "/expiry",
		f.base() + "/categories/" + id + "/shelf-life",
	} {
		t.Run(path, func(t *testing.T) {
			rec := f.anonymous(http.MethodPatch, path, `{}`)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "unauthorized", errorCode(t, rec))
		})
	}
}
