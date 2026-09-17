package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// fakeBackgrounds stands in for internal/ingest's Backgrounds. Its "cutout" is
// a fixed PNG, and it records the picture it was handed so a test can tell a
// crop from the whole photo.
type fakeBackgrounds struct {
	mu        sync.Mutex
	available bool
	err       error
	got       [][]byte
}

var cutoutBytes = []byte("\x89PNG\r\n\x1a\ncutout")

func (f *fakeBackgrounds) Available(context.Context) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "gemini-image-test", f.available
}

func (f *fakeBackgrounds) Remove(_ context.Context, picture []byte, _ images.Format) (*images.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, picture)
	if f.err != nil {
		return nil, f.err
	}
	return &images.Result{Data: cutoutBytes, Format: images.FormatPNG}, nil
}

// fakeCutouts is an in-memory CutoutStore.
type fakeCutouts struct {
	mu      sync.Mutex
	files   map[uuid.UUID]map[string][]byte
	removed []uuid.UUID
}

func (f *fakeCutouts) Save(job uuid.UUID, name string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files[job] == nil {
		f.files[job] = map[string][]byte{}
	}
	f.files[job][name] = data
	return nil
}

func (f *fakeCutouts) Read(job uuid.UUID, name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.files[job][name]; ok {
		return d, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeCutouts) RemoveAll(job uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, job)
	f.removed = append(f.removed, job)
	return nil
}

// backgroundFixture is an API fixture with background removal wired.
func backgroundFixture(t *testing.T) (*apiFixture, *fakeBackgrounds, *fakeCutouts) {
	t.Helper()
	backgrounds := &fakeBackgrounds{available: true}
	cutouts := &fakeCutouts{files: map[uuid.UUID]map[string][]byte{}}
	f := newAPIFixture(t, func(d *httpapi.Deps) {
		d.Backgrounds, d.Cutouts = backgrounds, cutouts
	})
	return f, backgrounds, cutouts
}

func cutoutsPath(f *apiFixture, job *store.Job) string {
	return f.base() + "/ingest/" + job.ID.String() + "/cutouts"
}

type cutoutCreated struct {
	CutoutID uuid.UUID `json:"cutout_id"`
	URL      string    `json:"url"`
}

func offersBackgroundRemoval(t *testing.T, f *apiFixture, job *store.Job) bool {
	t.Helper()
	rec := f.do(http.MethodGet, f.base()+"/jobs/"+job.ID.String(), "")
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		BackgroundRemoval *bool `json:"background_removal"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body.BackgroundRemoval, "always present, so a client never guesses")
	return *body.BackgroundRemoval
}

// TestAJobOffersBackgroundRemovalOnlyWhenItCanBeDone — the review screen
// renders the control from this flag alone, so it must be false for every job
// the cutout route would refuse, and for every deployment without the model
// (docs/specs/09-consumption-logging.md's acceptance criterion).
func TestAJobOffersBackgroundRemovalOnlyWhenItCanBeDone(t *testing.T) {
	t.Parallel()

	f, backgrounds, _ := backgroundFixture(t)
	review := f.reviewJob(t)
	assert.True(t, offersBackgroundRemoval(t, f, review))

	pending := f.photoJob(t, f.storageID, store.JobShelfIngestion, store.JobPending)
	assert.False(t, offersBackgroundRemoval(t, f, pending), "nothing to review yet")
	consumption := f.photoJob(t, f.storageID, store.JobConsumptionPhoto, store.JobDone)
	assert.False(t, offersBackgroundRemoval(t, f, consumption), "consumption never gives a product a picture")
	noPhoto := f.jobs.add(t, f.storageID, store.JobDone, `{"rows":[]}`)
	assert.False(t, offersBackgroundRemoval(t, f, noPhoto))

	backgrounds.mu.Lock()
	backgrounds.available = false
	backgrounds.mu.Unlock()
	assert.False(t, offersBackgroundRemoval(t, f, review), "a configured model the provider no longer lists")

	plain := newAPIFixture(t)
	assert.False(t, offersBackgroundRemoval(t, plain, plain.reviewJob(t)), "GEMINI_IMAGE_MODEL unset")
	assert.Equal(t, http.StatusNotFound,
		plain.do(http.MethodPost, cutoutsPath(plain, plain.reviewJob(t)), `{"row_id":"0","source":"crop"}`).Code,
		"and there is no route to call")
}

// TestCutoutIsMadeFromThePictureAConfirmWouldTake — the image model gets the
// same crop, or the same whole photo, that confirming without it would store,
// and the result is served back from the job.
func TestCutoutIsMadeFromThePictureAConfirmWouldTake(t *testing.T) {
	t.Parallel()

	f, backgrounds, cutouts := backgroundFixture(t)
	job := f.reviewJob(t)

	for source, size := range map[string][2]int{"crop": {20, 10}, "photo": {40, 20}} {
		backgrounds.mu.Lock()
		backgrounds.got = nil
		backgrounds.mu.Unlock()

		rec := f.do(http.MethodPost, cutoutsPath(f, job), `{"row_id":"0","source":"`+source+`"}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created cutoutCreated
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

		require.Len(t, backgrounds.got, 1, source)
		w, h := decodedSize(t, backgrounds.got[0])
		assert.Equal(t, size, [2]int{w, h}, source)

		assert.Equal(t, f.base()+"/ingest/"+job.ID.String()+"/cutouts/"+created.CutoutID.String(), created.URL)
		stored, err := cutouts.Read(job.ID, created.CutoutID.String()+".png")
		require.NoError(t, err, "kept with the job, not with any product")
		assert.Equal(t, cutoutBytes, stored)

		served := f.do(http.MethodGet, created.URL, "")
		require.Equal(t, http.StatusOK, served.Code)
		assert.Equal(t, "image/png", served.Header().Get("Content-Type"))
		assert.Equal(t, "nosniff", served.Header().Get("X-Content-Type-Options"))
		assert.Contains(t, served.Header().Get("Cache-Control"), "private")
		assert.Equal(t, cutoutBytes, served.Body.Bytes())
	}
	assert.Empty(t, f.pictures.files, "nothing is a product picture until a confirm says so")
}

// TestCutoutImageIsScopedToItsJobsStorage — a cutout is a picture of
// someone's home, served like the photo it came from.
func TestCutoutImageIsScopedToItsJobsStorage(t *testing.T) {
	t.Parallel()

	f, _, cutouts := backgroundFixture(t)
	foreign := f.jobs.add(t, uuid.New(), store.JobDone, `{"rows":[]}`)
	id := uuid.New()
	require.NoError(t, cutouts.Save(foreign.ID, id.String()+".png", cutoutBytes))

	foreignURL := f.base() + "/ingest/" + foreign.ID.String() + "/cutouts/" + id.String()
	assert.Equal(t, http.StatusNotFound, f.do(http.MethodGet, foreignURL, "").Code)

	mine := f.reviewJob(t)
	assert.Equal(t, http.StatusNotFound,
		f.do(http.MethodGet, f.base()+"/ingest/"+mine.ID.String()+"/cutouts/"+id.String(), "").Code,
		"another job's cutout id names nothing in this job")
	assert.Equal(t, http.StatusNotFound,
		f.do(http.MethodGet, f.base()+"/ingest/"+mine.ID.String()+"/cutouts/not-a-uuid", "").Code)
}

func TestCutoutRefusals(t *testing.T) {
	t.Parallel()

	f, _, cutouts := backgroundFixture(t)
	job := f.reviewJob(t)
	pending := f.photoJob(t, f.storageID, store.JobShelfIngestion, store.JobPending)
	consumption := f.photoJob(t, f.storageID, store.JobConsumptionPhoto, store.JobDone)
	foreign := f.photoJob(t, uuid.New(), store.JobShelfIngestion, store.JobDone)
	photoGone := f.reviewJob(t)
	delete(f.photos.files, *photoGone.ImageFilename)

	for name, tc := range map[string]struct {
		job    *store.Job
		body   string
		status int
	}{
		"unknown source":        {job, `{"row_id":"0","source":"upload"}`, http.StatusUnprocessableEntity},
		"no row":                {job, `{"source":"crop"}`, http.StatusUnprocessableEntity},
		"row not in proposal":   {job, `{"row_id":"7","source":"photo"}`, http.StatusUnprocessableEntity},
		"crop of a boxless row": {job, `{"row_id":"1","source":"crop"}`, http.StatusUnprocessableEntity},
		"photo gone":            {photoGone, `{"row_id":"0","source":"photo"}`, http.StatusUnprocessableEntity},
		"not under review":      {pending, `{"row_id":"0","source":"photo"}`, http.StatusConflict},
		"consumption job":       {consumption, `{"row_id":"0","source":"photo"}`, http.StatusNotFound},
		"another storage":       {foreign, `{"row_id":"0","source":"photo"}`, http.StatusNotFound},
	} {
		rec := f.do(http.MethodPost, cutoutsPath(f, tc.job), tc.body)
		assert.Equal(t, tc.status, rec.Code, name+": "+rec.Body.String())
	}
	assert.Empty(t, cutouts.files, "no refusal leaves a cutout behind")
}

// TestCutoutFailuresOfTheModel — withdrawn is the admin's configuration
// problem, anything else is a 502 whose provider detail stays in the log; the
// review screen keeps the original either way.
func TestCutoutFailuresOfTheModel(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		available bool
		err       error
		status    int
		code      string
	}{
		"not offered": {false, nil, http.StatusServiceUnavailable, httpapi.CodeModelUnavailable},
		"withdrawn":   {true, vision.ErrModelNotFound, http.StatusServiceUnavailable, httpapi.CodeModelUnavailable},
		"outage":      {true, errors.New("vision: generateContent returned 500 for key AIza-secret"), http.StatusBadGateway, httpapi.CodeUpstreamFailed},
		"unusable":    {true, images.ErrUnusableMask, http.StatusBadGateway, httpapi.CodeUpstreamFailed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, backgrounds, cutouts := backgroundFixture(t)
			backgrounds.available, backgrounds.err = tc.available, tc.err
			job := f.reviewJob(t)

			rec := f.do(http.MethodPost, cutoutsPath(f, job), `{"row_id":"0","source":"crop"}`)
			assert.Equal(t, tc.status, rec.Code)
			assert.Contains(t, rec.Body.String(), `"`+tc.code+`"`)
			assert.NotContains(t, rec.Body.String(), "AIza", "a provider's error text never reaches the client")
			assert.Empty(t, cutouts.files)
		})
	}
}

// TestConfirmKeepsTheCutoutTheReviewerChose — a confirm naming a cutout gives
// the product exactly that picture, and once the review is over no cutout of
// the job is kept.
func TestConfirmKeepsTheCutoutTheReviewerChose(t *testing.T) {
	t.Parallel()

	f, _, cutouts := backgroundFixture(t)
	job := f.reviewJob(t)

	rec := f.do(http.MethodPost, cutoutsPath(f, job), `{"row_id":"0","source":"crop"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created cutoutCreated
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	cutout := `"cutout","cutout_id":"` + created.CutoutID.String() + `"`
	rec = f.do(http.MethodPost, confirmPath(f, job), `{"items":[`+
		newProductRow("0", "Chutney", cutout)+`,{"row_id":"1","decision":"reject"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	url := f.ingest.decisions[0].NewProduct.ImageURL
	require.NotNil(t, url)
	prefix := "/api/storages/" + f.storageID.String() + "/product-images/"
	require.True(t, strings.HasPrefix(*url, prefix), *url)
	name := strings.TrimPrefix(*url, prefix)
	assert.True(t, strings.HasSuffix(name, ".png"), "a cutout stays a transparent PNG")
	assert.Equal(t, cutoutBytes, f.pictures.files[name], "the picture the reviewer saw, not a new cut")

	assert.Contains(t, cutouts.removed, job.ID, "the review is over")
	_, err := cutouts.Read(job.ID, created.CutoutID.String()+".png")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestConfirmRefusesACutoutItCannotUse(t *testing.T) {
	t.Parallel()

	f, _, cutouts := backgroundFixture(t)
	job := f.reviewJob(t)
	otherJob := f.reviewJob(t)
	elsewhere := uuid.New()
	require.NoError(t, cutouts.Save(otherJob.ID, elsewhere.String()+".png", cutoutBytes))

	for name, tc := range map[string]struct {
		image string
		field string
	}{
		"never made":           {`"cutout","cutout_id":"` + uuid.NewString() + `"`, "items[0].new_product.cutout_id"},
		"another job's cutout": {`"cutout","cutout_id":"` + elsewhere.String() + `"`, "items[0].new_product.cutout_id"},
		"no id":                {`"cutout"`, "items[0].new_product.cutout_id"},
		"id without a cutout":  {`"crop","cutout_id":"` + elsewhere.String() + `"`, "items[0].new_product.cutout_id"},
	} {
		rec := f.do(http.MethodPost, confirmPath(f, job), `{"items":[`+
			newProductRow("0", "Chutney", tc.image)+`,{"row_id":"1","decision":"reject"}]}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, name+": "+rec.Body.String())
		assert.Contains(t, rec.Body.String(), tc.field, name)
	}
	assert.Empty(t, f.pictures.files)
	assert.NotContains(t, cutouts.removed, job.ID, "a refused confirm leaves the review as it was")
}

// TestAdminBannerNamesAnImageModelTheProviderDropped — listed only when
// GEMINI_IMAGE_MODEL is configured and not offered; a deployment that never
// configured one hears nothing about it.
func TestAdminBannerNamesAnImageModelTheProviderDropped(t *testing.T) {
	t.Parallel()

	const warning = "background-removal model"
	adminPage := func(f *apiFixture) string {
		f.auth.setAdmin(f.user.ID, true)
		f.user.IsAdmin = true
		rec := f.do(http.MethodGet, "/admin", "")
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}

	f, backgrounds, _ := backgroundFixture(t)
	assert.NotContains(t, adminPage(f), warning, "available: nothing to say")

	backgrounds.mu.Lock()
	backgrounds.available = false
	backgrounds.mu.Unlock()
	page := adminPage(f)
	assert.Contains(t, page, warning)
	assert.Contains(t, page, "gemini-image-test")

	assert.NotContains(t, adminPage(newAPIFixture(t)), warning, "never configured")
}

// TestDiscardAndReanalyzeClearAJobsCutouts — cutouts belong to one proposal:
// discarding the job drops them, and so does replacing its proposal.
func TestDiscardAndReanalyzeClearAJobsCutouts(t *testing.T) {
	t.Parallel()

	f, _, cutouts := backgroundFixture(t)

	reanalyzed := f.reviewJob(t)
	require.Equal(t, http.StatusCreated, f.do(http.MethodPost, cutoutsPath(f, reanalyzed), `{"row_id":"0","source":"photo"}`).Code)
	require.Equal(t, http.StatusAccepted, f.do(http.MethodPost, reanalyzePath(f, reanalyzed), "").Code)
	assert.Empty(t, cutouts.files[reanalyzed.ID], "the rows they were cut for are gone")

	discarded := f.reviewJob(t)
	require.Equal(t, http.StatusCreated, f.do(http.MethodPost, cutoutsPath(f, discarded), `{"row_id":"0","source":"photo"}`).Code)
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.base()+"/jobs/"+discarded.ID.String(), "").Code)
	assert.Empty(t, cutouts.files[discarded.ID])
}
