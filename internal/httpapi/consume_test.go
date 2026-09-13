package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/consume"
	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeConsumer is an in-memory Consumer, mirroring fakeIngester in
// ingest_test.go.
type fakeConsumer struct {
	mu        sync.Mutex
	available bool
	started   []consume.Upload
	err       error
}

func (f *fakeConsumer) Available(context.Context) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "gemini-test", f.available
}

func (f *fakeConsumer) Start(_ context.Context, u consume.Upload) (*store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.started = append(f.started, u)
	return &store.Job{ID: uuid.New(), StorageID: u.StorageID, Kind: store.JobConsumptionPhoto, Status: store.JobPending}, nil
}

// fakeConsumeStore is an in-memory ConsumeStore, mirroring fakeIngestStore.
type fakeConsumeStore struct {
	mu        sync.Mutex
	decisions []store.ConsumeDecision
	jobID     uuid.UUID
	storageID uuid.UUID
	userID    *uuid.UUID
	err       error
}

func (f *fakeConsumeStore) ConfirmConsumption(_ context.Context, storageID, jobID uuid.UUID, userID *uuid.UUID, decisions []store.ConsumeDecision) (*store.ConsumeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storageID, f.jobID, f.userID, f.decisions = storageID, jobID, userID, decisions
	if f.err != nil {
		return nil, f.err
	}
	return &store.ConsumeResult{BatchIDs: []uuid.UUID{uuid.New()}}, nil
}

// TestConsumeUploadAnswers202WithAJobAndAStrippedPhoto — the same shape as
// shelf and product ingestion: at once, the stripped photo under a generated
// name, never what the client called it, and no location hint — consumption
// decrements batches that already have a location.
func TestConsumeUploadAnswers202WithAJobAndAStrippedPhoto(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	original := jpegWithEXIF(t, 8, 4, 6)

	rec := f.photoUpload(t, f.base()+"/consume/photos", original, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var body struct {
		JobID uuid.UUID `json:"job_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotEqual(t, uuid.Nil, body.JobID)

	require.Len(t, f.consumer.started, 1)
	started := f.consumer.started[0]
	assert.Equal(t, f.storageID, started.StorageID, "the storage comes from the gate, never the form")
	assert.Equal(t, f.user.ID, started.CreatedBy)
	assert.NotContains(t, started.Filename, "IMG_0001")
	assert.NotContains(t, string(started.Image), "Exif\x00\x00", "metadata is stripped before the photo is kept")
}

func TestConsumeUploadWithAnUnavailableModelIs503(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.consumer.available = false

	rec := f.photoUpload(t, f.base()+"/consume/photos", jpegWithEXIF(t, 4, 4, 1), nil)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "model_unavailable")
	assert.Empty(t, f.consumer.started, "no job for a photo that cannot be analysed")
}

func TestConsumeUploadRefusals(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	path := f.base() + "/consume/photos"

	assert.Equal(t, http.StatusUnprocessableEntity, f.photoUpload(t, path, nil, nil).Code, "no image")
	assert.Equal(t, http.StatusUnprocessableEntity, f.photoUpload(t, path, []byte("GIF89a not a photo"), nil).Code, "not JPEG or PNG")

	_, outsiderSession := f.auth.addUser(t, false)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: outsiderSession.ID})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "a non-member cannot upload into the storage")
}

// TestConsumeConfirmTranslatesTheBodyIntoDecisions — an accepted row names an
// existing product and at least one batch decrement; a rejected row carries
// neither.
func TestConsumeConfirmTranslatesTheBodyIntoDecisions(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	jobID := uuid.New()
	product, batchA, batchB := uuid.New(), uuid.New(), uuid.New()

	rec := f.do(http.MethodPost, f.base()+"/consume/photos/"+jobID.String()+"/confirm", `{"items":[
	  {"row_id":"0","decision":"accept","product_id":"`+product.String()+`","decrements":[
	    {"batch_id":"`+batchA.String()+`","quantity":2},
	    {"batch_id":"`+batchB.String()+`","quantity":1}
	  ]},
	  {"row_id":"1","decision":"reject"}
	]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "batch_ids")

	d := f.consume.decisions
	require.Len(t, d, 2)
	assert.Equal(t, jobID, f.consume.jobID)
	assert.Equal(t, f.storageID, f.consume.storageID)
	assert.Equal(t, f.user.ID, *f.consume.userID)

	assert.True(t, d[0].Accept)
	require.Equal(t, &product, d[0].ProductID)
	require.Len(t, d[0].Decrements, 2)
	assert.Equal(t, batchA, d[0].Decrements[0].BatchID)
	assert.Equal(t, 2, d[0].Decrements[0].Quantity)
	assert.Equal(t, batchB, d[0].Decrements[1].BatchID)
	assert.Equal(t, 1, d[0].Decrements[1].Quantity)

	assert.False(t, d[1].Accept)
	assert.Nil(t, d[1].ProductID)
}

// TestConsumeConfirmValidatesEachRow — the same body-shape validation as
// ingestion's confirm, adapted to consumption's fields: a product is always
// required on accept (consumption never creates one), and at least one valid
// batch decrement is required.
func TestConsumeConfirmValidatesEachRow(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	path := f.base() + "/consume/photos/" + uuid.NewString() + "/confirm"
	id := uuid.NewString()

	for name, tc := range map[string]struct{ body, field string }{
		"no items":                 {`{}`, "items"},
		"bad decision":             {`{"items":[{"row_id":"0","decision":"maybe"}]}`, "items[0].decision"},
		"no product":               {`{"items":[{"row_id":"0","decision":"accept","decrements":[{"batch_id":"` + id + `","quantity":1}]}]}`, "items[0].product_id"},
		"no decrements":            {`{"items":[{"row_id":"0","decision":"accept","product_id":"` + id + `","decrements":[]}]}`, "items[0].decrements"},
		"missing decrements field": {`{"items":[{"row_id":"0","decision":"accept","product_id":"` + id + `"}]}`, "items[0].decrements"},
		"zero decrement quantity":  {`{"items":[{"row_id":"0","decision":"accept","product_id":"` + id + `","decrements":[{"batch_id":"` + id + `","quantity":0}]}]}`, "items[0].decrements"},
		"missing row id on reject": {`{"items":[{"decision":"reject"}]}`, "items[0].row_id"},
	} {
		rec := f.do(http.MethodPost, path, tc.body)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, name)
		assert.Contains(t, rec.Body.String(), `"`+tc.field+`"`, name)
	}
	assert.Nil(t, f.consume.decisions, "nothing reaches the store while the body is invalid")
}

func TestConsumeConfirmMapsStoreRefusals(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	path := f.base() + "/consume/photos/" + uuid.NewString() + "/confirm"
	body := `{"items":[{"row_id":"0","decision":"reject"}]}`

	for err, status := range map[error]int{
		store.ErrValidation: http.StatusUnprocessableEntity,
		store.ErrConflict:   http.StatusConflict,
		store.ErrNotFound:   http.StatusNotFound,
	} {
		f.consume.err = err
		rec := f.do(http.MethodPost, path, body)
		assert.Equal(t, status, rec.Code, err.Error())
		assert.NotContains(t, rec.Body.String(), "store:", "internal text stays out of a production response")
	}

	assert.Equal(t, http.StatusNotFound, f.do(http.MethodPost, f.base()+"/consume/photos/not-a-uuid/confirm", body).Code)
}
