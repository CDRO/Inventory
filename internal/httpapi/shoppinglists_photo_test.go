package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// The two photo shapes of POST /shopping-lists: a multipart upload
// (docs/specs/07-shopping-list-reconciliation.md's own contract, which had
// stood unimplemented at 501) and from_job_id, the reclassification of
// docs/specs/41-mixed-photo-classification.md.

// classifiedPayload is what an analysis writes when it spotted a list on a
// photo somebody uploaded as something else.
func classifiedPayload(lines ...string) string {
	encoded, err := json.Marshal(lines)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"mode":"shelf","rows":[],"looks_like_shopping_list":true,"shopping_list_lines":%s}`, encoded)
}

// classifiedJob seeds a done job of the given kind whose analysis flagged a
// shopping list, and returns it.
func classifiedJob(t *testing.T, f *apiFixture, kind store.JobKind, payload string) *store.Job {
	t.Helper()
	job := f.jobs.add(t, f.storageID, store.JobDone, payload)
	job.Kind = kind
	name := job.ID.String() + ".jpg"
	job.ImageFilename = &name
	require.NoError(t, f.photos.Save(name, []byte("stripped jpeg")))
	return job
}

func TestReclassifyingAPhotoCreatesTheListAndDiscardsTheJob(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := classifiedJob(t, f, store.JobShelfIngestion, classifiedPayload("milk", "eggs x2"))

	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"from_job_id":"`+job.ID.String()+`"}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var list struct {
		ID     uuid.UUID `json:"id"`
		Source string    `json:"source"`
		Items  []struct {
			RawText string `json:"raw_text"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Equal(t, "photo", list.Source, "the list remembers it came from a photo")
	require.Len(t, list.Items, 2)
	assert.Equal(t, "milk", list.Items[0].RawText)
	assert.Equal(t, "eggs x2", list.Items[1].RawText)

	// Every line went through the shared matching service, with the quantity
	// parsed off exactly as a typed list's is.
	assert.Equal(t, []string{"milk", "eggs"}, f.matcher.texts)

	// No second analysis: the lines came from the one this job already paid
	// for (docs/specs/41-mixed-photo-classification.md).
	assert.Empty(t, f.ingester.started, "no upload")

	// The origin job is discarded — the row is gone, and its photo with it.
	_, err := f.jobs.Job(t.Context(), f.storageID, job.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Empty(t, f.photos.files, "a discarded job's photo goes too")
}

// Reclassifying is not idempotent: the job is discarded by it, so a second
// attempt is the same 404 an id that never existed gets.
func TestReclassifyingTwiceIsNotFound(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := classifiedJob(t, f, store.JobShelfIngestion, classifiedPayload("milk"))
	body := `{"from_job_id":"` + job.ID.String() + `"}`

	require.Equal(t, http.StatusCreated, f.do(http.MethodPost, f.base()+"/shopping-lists", body).Code)

	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", body)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 1, f.lists.fromJobCalls, "the second attempt never reaches the store")
}

// Every way a job fails to qualify is the same 404, so a client cannot learn
// from the answer whether the job exists, belongs to this storage, or simply
// was not a list (docs/specs/03-auth-and-multi-tenancy.md).
func TestOnlyAClassifiedJobCanBecomeAList(t *testing.T) {
	t.Parallel()

	for name, seed := range map[string]func(t *testing.T, f *apiFixture) string{
		"no such job": func(*testing.T, *apiFixture) string {
			return uuid.New().String()
		},
		"malformed id": func(*testing.T, *apiFixture) string {
			return "not-a-uuid"
		},
		"another storage's job": func(t *testing.T, f *apiFixture) string {
			other := f.jobs.add(t, uuid.New(), store.JobDone, classifiedPayload("milk"))
			return other.ID.String()
		},
		"not classified as a list": func(t *testing.T, f *apiFixture) string {
			job := classifiedJob(t, f, store.JobShelfIngestion,
				`{"mode":"shelf","rows":[],"looks_like_shopping_list":false,"shopping_list_lines":[]}`)
			return job.ID.String()
		},
		"flagged but with no lines": func(t *testing.T, f *apiFixture) string {
			job := classifiedJob(t, f, store.JobShelfIngestion,
				`{"mode":"shelf","rows":[],"looks_like_shopping_list":true,"shopping_list_lines":[]}`)
			return job.ID.String()
		},
		"still being analysed": func(t *testing.T, f *apiFixture) string {
			job := f.jobs.add(t, f.storageID, store.JobPending, "")
			return job.ID.String()
		},
		"already applied": func(t *testing.T, f *apiFixture) string {
			job := classifiedJob(t, f, store.JobShelfIngestion, classifiedPayload("milk"))
			job.Status = store.JobConsumed
			return job.ID.String()
		},
		// A photo uploaded as a list has no misclassification to correct, and
		// its payload names a list that already exists ("No reverse check").
		"a shopping-list photo": func(t *testing.T, f *apiFixture) string {
			job := classifiedJob(t, f, store.JobShoppingListPhoto, `{"shopping_list_id":"`+uuid.New().String()+`"}`)
			return job.ID.String()
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			id := seed(t, f)

			rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"from_job_id":"`+id+`"}`)

			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.Equal(t, "not_found", errorCode(t, rec))
			assert.Zero(t, f.lists.lastCreated, "nothing is written for a job that does not qualify")
		})
	}
}

// The classification lives in the payload every review screen already reads,
// so a consumption photo reaches it through its own review screen the same
// way a shelf photo does.
func TestEveryPhysicalJobKindCanBeReclassified(t *testing.T) {
	t.Parallel()

	for _, kind := range []store.JobKind{store.JobShelfIngestion, store.JobProductPhoto, store.JobConsumptionPhoto} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			job := classifiedJob(t, f, kind, classifiedPayload("milk"))

			rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"from_job_id":"`+job.ID.String()+`"}`)

			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		})
	}
}

// source is optional on the reclassification body, but it cannot contradict
// what the request plainly is.
func TestReclassifyingRefusesAContradictorySource(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	job := classifiedJob(t, f, store.JobShelfIngestion, classifiedPayload("milk"))

	rec := f.do(http.MethodPost, f.base()+"/shopping-lists",
		`{"source":"text","from_job_id":"`+job.ID.String()+`"}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, 0, f.lists.fromJobCalls)
}

func TestPhotographedListUploadStartsAJob(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.photoUpload(t, f.base()+"/shopping-lists", jpegWithEXIF(t, 8, 4, 6), nil)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body struct {
		JobID uuid.UUID `json:"job_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotEqual(t, uuid.Nil, body.JobID)

	require.Len(t, f.ingester.started, 1)
	started := f.ingester.started[0]
	assert.Equal(t, store.JobShoppingListPhoto, started.Kind)
	assert.Equal(t, f.storageID, started.StorageID)
	// Stripped before the photo was ever written: what reaches the service is
	// already the clean image (docs/specs/04-backend-api-conventions.md).
	assert.NotContains(t, string(started.Image), "Exif")
	assert.Empty(t, f.lists.lastCreated, "the list is written by the job, not by the upload")
}

// The model check runs before the body is read, so a deployment whose model
// has been withdrawn says so instead of taking someone's photo first.
func TestPhotographedListNeedsAUsableModel(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.ingester.available = false

	rec := f.photoUpload(t, f.base()+"/shopping-lists", jpegWithEXIF(t, 4, 4, 1), nil)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "model_unavailable")
	assert.Empty(t, f.ingester.started)
}

func TestPhotographedListRefusesWhatIsNotAPhoto(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	path := f.base() + "/shopping-lists"

	assert.Equal(t, http.StatusUnprocessableEntity, f.photoUpload(t, path, nil, nil).Code, "no image")
	assert.Equal(t, http.StatusUnprocessableEntity,
		f.photoUpload(t, path, []byte("GIF89a not a photo"), nil).Code, "not JPEG or PNG")
	assert.Empty(t, f.ingester.started)
}

// An unusable upload volume is the server's problem, not the uploader's.
func TestPhotographedListWithoutAnIngesterIsAnInternalError(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, func(d *httpapi.Deps) { d.Ingester = nil })

	rec := f.photoUpload(t, f.base()+"/shopping-lists", jpegWithEXIF(t, 4, 4, 1), nil)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// The text path is untouched by any of this.
func TestTypedListsStillWorkUnchanged(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"source":"text","raw_text":"milk\neggs x2"}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"milk", "eggs"}, f.matcher.texts)
	assert.Equal(t, 0, f.lists.fromJobCalls)
}
