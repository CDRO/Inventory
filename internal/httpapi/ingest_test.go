package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/ingest"
	"github.com/CDRO/Inventory/internal/store"
)

type fakeIngester struct {
	mu        sync.Mutex
	available bool
	started   []ingest.Upload
	err       error
}

func (f *fakeIngester) Available(context.Context) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "gemini-test", f.available
}

func (f *fakeIngester) Start(_ context.Context, u ingest.Upload) (*store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.started = append(f.started, u)
	return &store.Job{ID: uuid.New(), StorageID: u.StorageID, Kind: u.Kind, Status: store.JobPending}, nil
}

type fakeIngestStore struct {
	mu        sync.Mutex
	decisions []store.IngestDecision
	jobID     uuid.UUID
	storageID uuid.UUID
	userID    *uuid.UUID
	err       error
	// usedImages is what ProductImageInStorage answers from, keyed by storage
	// id and image URL: every picture a successful confirm gave a product.
	usedImages map[string]bool
}

func (f *fakeIngestStore) ConfirmIngestion(_ context.Context, storageID, jobID uuid.UUID, userID *uuid.UUID, decisions []store.IngestDecision) (*store.IngestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storageID, f.jobID, f.userID, f.decisions = storageID, jobID, userID, decisions
	if f.err != nil {
		return nil, f.err
	}

	// Like the real store, rows naming the same new product create it once,
	// with the first of those rows' pictures.
	created := map[string]bool{}
	for _, d := range decisions {
		if !d.Accept || d.NewProduct == nil {
			continue
		}
		key := store.NormalizeCatalogName(d.NewProduct.Name)
		if created[key] {
			continue
		}
		created[key] = true
		if d.NewProduct.ImageURL != nil {
			f.markImageUsed(storageID, *d.NewProduct.ImageURL)
		}
	}
	return &store.IngestResult{BatchIDs: []uuid.UUID{uuid.New()}, ProductsCreated: len(created)}, nil
}

// markImageUsed records a picture as some product's in storageID. Callers
// hold f.mu, or are a test setting up before any request runs.
func (f *fakeIngestStore) markImageUsed(storageID uuid.UUID, url string) {
	if f.usedImages == nil {
		f.usedImages = map[string]bool{}
	}
	f.usedImages[storageID.String()+" "+url] = true
}

func (f *fakeIngestStore) ProductImageInStorage(_ context.Context, storageID uuid.UUID, url string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usedImages[storageID.String()+" "+url], nil
}

type fakePhotoStore struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (f *fakePhotoStore) Save(name string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = data
	return nil
}

func (f *fakePhotoStore) Read(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.files[name]; ok {
		return d, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakePhotoStore) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, name)
	return nil
}

// photoUpload builds a multipart ingest request as the fixture's member.
func (f *apiFixture) photoUpload(t *testing.T, path string, image []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if image != nil {
		part, err := writer.CreateFormFile("image", "IMG_0001.jpg")
		require.NoError(t, err)
		_, err = part.Write(image)
		require.NoError(t, err)
	}
	for k, v := range fields {
		require.NoError(t, writer.WriteField(k, v))
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: f.session.ID})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// TestUploadAnswers202WithAJobAndAStrippedPhoto — the upload returns at once;
// the photo handed on is the stripped one under a generated name, never what
// the client called it.
func TestUploadAnswers202WithAJobAndAStrippedPhoto(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	original := jpegWithEXIF(t, 8, 4, 6)
	hint := uuid.New()

	for path, kind := range map[string]store.JobKind{
		f.base() + "/ingest/shelf-photos":   store.JobShelfIngestion,
		f.base() + "/ingest/product-photos": store.JobProductPhoto,
	} {
		rec := f.photoUpload(t, path, original, map[string]string{"location_id": hint.String()})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		var body struct {
			JobID uuid.UUID `json:"job_id"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.NotEqual(t, uuid.Nil, body.JobID)

		started := f.ingester.started[len(f.ingester.started)-1]
		assert.Equal(t, kind, started.Kind)
		assert.Equal(t, f.storageID, started.StorageID, "the storage comes from the gate, never the form")
		assert.Equal(t, f.user.ID, started.CreatedBy)
		assert.Equal(t, &hint, started.LocationHintID)
		assert.NotContains(t, started.Filename, "IMG_0001")
		assert.True(t, strings.HasSuffix(started.Filename, ".jpg"))
		assert.NotContains(t, string(started.Image), "Exif\x00\x00", "metadata is stripped before the photo is kept")
	}
}

// TestUploadWithAnUnavailableModelIs503 — a configuration problem for the
// admin, reported before the photo is read, not a failed photo later.
func TestUploadWithAnUnavailableModelIs503(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.ingester.available = false

	rec := f.photoUpload(t, f.base()+"/ingest/shelf-photos", jpegWithEXIF(t, 4, 4, 1), nil)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "model_unavailable")
	assert.Empty(t, f.ingester.started, "no job for a photo that cannot be analysed")
}

func TestUploadRefusals(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	path := f.base() + "/ingest/shelf-photos"

	assert.Equal(t, http.StatusUnprocessableEntity, f.photoUpload(t, path, nil, nil).Code, "no image")
	assert.Equal(t, http.StatusUnprocessableEntity, f.photoUpload(t, path, []byte("GIF89a not a photo"), nil).Code, "not JPEG or PNG")

	malformedHint := f.photoUpload(t, path, jpegWithEXIF(t, 4, 4, 1), map[string]string{"location_id": "shelf-3"})
	assert.Equal(t, http.StatusNotFound, malformedHint.Code)

	f.ingester.err = store.ErrNotFound
	foreignHint := f.photoUpload(t, path, jpegWithEXIF(t, 4, 4, 1), map[string]string{"location_id": uuid.NewString()})
	assert.Equal(t, http.StatusNotFound, foreignHint.Code, "a shelf in another storage is the same 404")
	assert.Equal(t, malformedHint.Body.String(), foreignHint.Body.String())

	_, outsider := f.auth.addUser(t, false)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: outsider.ID})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "a non-member cannot upload into the storage")
}

func TestConfirmTranslatesTheBodyIntoDecisions(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	jobID := uuid.New()
	product, location, category, parent := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	rec := f.do(http.MethodPost, f.base()+"/ingest/"+jobID.String()+"/confirm", `{"items":[
	  {"row_id":"0","decision":"accept","product_id":"`+product.String()+`","quantity":3,"location_id":"`+location.String()+`"},
	  {"row_id":"1","decision":"accept","new_product":{"name":" Chutney ","category_id":"`+category.String()+`","item_type":"perishable"},
	   "quantity":1,"new_location":{"parent_id":"`+parent.String()+`","names":["Top Shelf"]},"expiration_date":"2027-01-31"},
	  {"row_id":"2","decision":"accept","product_id":"`+product.String()+`","quantity":1,"location_id":"`+location.String()+`","expiration_date":null},
	  {"row_id":"3","decision":"reject"}
	]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"products_created":1`)

	d := f.ingest.decisions
	require.Len(t, d, 4)
	assert.Equal(t, jobID, f.ingest.jobID)
	assert.Equal(t, f.storageID, f.ingest.storageID)
	assert.Equal(t, f.user.ID, *f.ingest.userID)

	assert.True(t, d[0].Accept)
	assert.Equal(t, &product, d[0].ProductID)
	assert.False(t, d[0].ExpirationEdited, "an absent date accepts the default")

	require.NotNil(t, d[1].NewProduct)
	assert.Equal(t, "Chutney", d[1].NewProduct.Name)
	assert.Equal(t, store.ItemPerishable, d[1].NewProduct.ItemType)
	require.NotNil(t, d[1].NewLocation)
	assert.Equal(t, []string{"Top Shelf"}, d[1].NewLocation.Names)
	assert.True(t, d[1].ExpirationEdited)
	require.NotNil(t, d[1].ExpirationDate)
	assert.Equal(t, time.Date(2027, time.January, 31, 0, 0, 0, 0, time.UTC), *d[1].ExpirationDate)

	assert.True(t, d[2].ExpirationEdited, "an explicit null is the reviewer saying it does not expire")
	assert.Nil(t, d[2].ExpirationDate)

	assert.False(t, d[3].Accept)
}

func TestConfirmValidatesEachRow(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	path := f.base() + "/ingest/" + uuid.NewString() + "/confirm"
	id := uuid.NewString()

	for name, tc := range map[string]struct{ body, field string }{
		"no items":                 {`{}`, "items"},
		"bad decision":             {`{"items":[{"row_id":"0","decision":"maybe"}]}`, "items[0].decision"},
		"both product kinds":       {`{"items":[{"row_id":"0","decision":"accept","product_id":"` + id + `","new_product":{"name":"x"},"quantity":1,"location_id":"` + id + `"}]}`, "items[0].product_id"},
		"no product":               {`{"items":[{"row_id":"0","decision":"accept","quantity":1,"location_id":"` + id + `"}]}`, "items[0].product_id"},
		"zero quantity":            {`{"items":[{"row_id":"0","decision":"accept","product_id":"` + id + `","quantity":0,"location_id":"` + id + `"}]}`, "items[0].quantity"},
		"no location":              {`{"items":[{"row_id":"0","decision":"accept","product_id":"` + id + `","quantity":1}]}`, "items[0].location_id"},
		"blank new product name":   {`{"items":[{"row_id":"0","decision":"accept","new_product":{"name":"  "},"quantity":1,"location_id":"` + id + `"}]}`, "items[0].new_product.name"},
		"bad item type":            {`{"items":[{"row_id":"0","decision":"accept","new_product":{"name":"x","item_type":"liquid"},"quantity":1,"location_id":"` + id + `"}]}`, "items[0].new_product.item_type"},
		"empty new location":       {`{"items":[{"row_id":"0","decision":"accept","product_id":"` + id + `","quantity":1,"new_location":{"names":[]}}]}`, "items[0].new_location.names"},
		"bad date":                 {`{"items":[{"row_id":"0","decision":"accept","product_id":"` + id + `","quantity":1,"location_id":"` + id + `","expiration_date":"31/01/2027"}]}`, "items[0].expiration_date"},
		"missing row id on reject": {`{"items":[{"decision":"reject"}]}`, "items[0].row_id"},
	} {
		rec := f.do(http.MethodPost, path, tc.body)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, name)
		assert.Contains(t, rec.Body.String(), `"`+tc.field+`"`, name)
	}
	assert.Nil(t, f.ingest.decisions, "nothing reaches the store while the body is invalid")
}

func TestConfirmMapsStoreRefusals(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	path := f.base() + "/ingest/" + uuid.NewString() + "/confirm"
	body := `{"items":[{"row_id":"0","decision":"reject"}]}`

	for err, status := range map[error]int{
		store.ErrValidation: http.StatusUnprocessableEntity,
		store.ErrConflict:   http.StatusConflict,
		store.ErrNotFound:   http.StatusNotFound,
	} {
		f.ingest.err = err
		rec := f.do(http.MethodPost, path, body)
		assert.Equal(t, status, rec.Code, err.Error())
		assert.NotContains(t, rec.Body.String(), "store:", "internal text stays out of a production response")
	}

	assert.Equal(t, http.StatusNotFound, f.do(http.MethodPost, f.base()+"/ingest/not-a-uuid/confirm", body).Code)
}

// TestJobImageIsStorageScopedAndDiscardedWithTheJob — the review renders crops
// from this route, and discarding a job drops its photo.
func TestJobImageIsStorageScopedAndDiscardedWithTheJob(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	name := "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.jpg"
	f.photos.files[name] = []byte("\xFF\xD8\xFFjpeg")

	job := f.jobs.add(t, f.storageID, store.JobDone, `{"rows":[]}`)
	job.ImageFilename = &name
	foreign := f.jobs.add(t, uuid.New(), store.JobDone, `{"rows":[]}`)
	foreign.ImageFilename = &name

	rec := f.do(http.MethodGet, f.base()+"/jobs/"+job.ID.String()+"/image", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/jpeg", rec.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, rec.Header().Get("Cache-Control"), "private")
	assert.Equal(t, "\xFF\xD8\xFFjpeg", rec.Body.String())

	assert.Equal(t, http.StatusNotFound, f.do(http.MethodGet, f.base()+"/jobs/"+foreign.ID.String()+"/image", "").Code,
		"another storage's photo is a 404 like any other foreign id")

	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.base()+"/jobs/"+job.ID.String(), "").Code)
	_, err := f.photos.Read(name)
	assert.ErrorIs(t, err, os.ErrNotExist, "discarding a job drops its photo")
}
