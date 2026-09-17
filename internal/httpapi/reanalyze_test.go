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

// photoJob seeds a job of kind in status with a photo, in storageID.
func (f *apiFixture) photoJob(t *testing.T, storageID uuid.UUID, kind store.JobKind, status store.JobStatus) *store.Job {
	t.Helper()
	job := f.jobs.add(t, storageID, status, `{"rows":[]}`)
	name := uuid.Must(uuid.NewV7()).String() + ".jpg"
	job.Kind, job.ImageFilename = kind, &name
	return job
}

func reanalyzePath(f *apiFixture, job *store.Job) string {
	return f.base() + "/jobs/" + job.ID.String() + "/reanalyze"
}

// TestReanalyzeHandsAFinishedJobToItsOwnService — a done or failed job goes
// back to the service that created its kind, and the answer is the same job's
// id, not a new one.
func TestReanalyzeHandsAFinishedJobToItsOwnService(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	shelf := f.photoJob(t, f.storageID, store.JobShelfIngestion, store.JobDone)
	product := f.photoJob(t, f.storageID, store.JobProductPhoto, store.JobFailed)
	consumption := f.photoJob(t, f.storageID, store.JobConsumptionPhoto, store.JobDone)

	for _, job := range []*store.Job{shelf, product, consumption} {
		rec := f.do(http.MethodPost, reanalyzePath(f, job), "")
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		var body struct {
			JobID uuid.UUID `json:"job_id"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, job.ID, body.JobID)
	}

	assert.Equal(t, []uuid.UUID{shelf.ID, product.ID}, f.ingester.reanalyzed)
	assert.Equal(t, []uuid.UUID{consumption.ID}, f.consumer.reanalyzed)
}

// TestReanalyzeRefusesAJobItCannotAnalyseAgain — every refusal is decided
// before any service is asked, and a foreign job is the usual 404.
func TestReanalyzeRefusesAJobItCannotAnalyseAgain(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	noPhoto := f.jobs.add(t, f.storageID, store.JobDone, `{"rows":[]}`)
	for name, tc := range map[string]struct {
		job    *store.Job
		status int
	}{
		"still being analysed": {f.photoJob(t, f.storageID, store.JobShelfIngestion, store.JobPending), http.StatusConflict},
		"already applied":      {f.photoJob(t, f.storageID, store.JobShelfIngestion, store.JobConsumed), http.StatusConflict},
		"no photo":             {noPhoto, http.StatusConflict},
		"nothing to repeat":    {f.photoJob(t, f.storageID, store.JobShoppingListPhoto, store.JobDone), http.StatusConflict},
		"another storage":      {f.photoJob(t, uuid.New(), store.JobShelfIngestion, store.JobDone), http.StatusNotFound},
	} {
		rec := f.do(http.MethodPost, reanalyzePath(f, tc.job), "")
		assert.Equal(t, tc.status, rec.Code, name)
	}
	assert.Equal(t, http.StatusNotFound, f.do(http.MethodPost, f.base()+"/jobs/not-a-uuid/reanalyze", "").Code)

	assert.Empty(t, f.ingester.reanalyzed)
	assert.Empty(t, f.consumer.reanalyzed)
}

// TestReanalyzeWithAnUnavailableModelIs503 — like an upload, a withdrawn model
// is a configuration problem for the admin, and the job is left as it was.
func TestReanalyzeWithAnUnavailableModelIs503(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.consumer.available = false
	job := f.photoJob(t, f.storageID, store.JobConsumptionPhoto, store.JobDone)

	rec := f.do(http.MethodPost, reanalyzePath(f, job), "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), httpapi.CodeModelUnavailable)
	assert.Empty(t, f.consumer.reanalyzed)
}

// TestReanalyzeMapsTheStoresRefusal — a race lost after the handler's own
// checks still ends as the right status.
func TestReanalyzeMapsTheStoresRefusal(t *testing.T) {
	t.Parallel()

	for err, status := range map[error]int{
		fmt.Errorf("%w: job is consumed, not done or failed", store.ErrConflict): http.StatusConflict,
		store.ErrNotFound: http.StatusNotFound,
	} {
		f := newAPIFixture(t)
		f.ingester.err = err
		job := f.photoJob(t, f.storageID, store.JobShelfIngestion, store.JobDone)

		rec := f.do(http.MethodPost, reanalyzePath(f, job), "")
		assert.Equal(t, status, rec.Code, err.Error())
		assert.NotContains(t, rec.Body.String(), "store:", "internal text stays out of a production response")
	}
}

// TestReanalyzeWithoutAWiredServiceRefuses — a deployment whose upload volume
// was unusable has no service to analyse a shelf photo with.
func TestReanalyzeWithoutAWiredServiceRefuses(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, func(d *httpapi.Deps) { d.Ingester = nil })
	job := f.photoJob(t, f.storageID, store.JobShelfIngestion, store.JobDone)

	assert.Equal(t, http.StatusConflict, f.do(http.MethodPost, reanalyzePath(f, job), "").Code)
}
