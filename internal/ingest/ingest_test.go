package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/jobs"
	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// --- fakes ---

type fakeRunner struct {
	submitted   []store.NewJob
	resubmitted []uuid.UUID
	work        jobs.Work
	err         error
}

func (f *fakeRunner) Submit(_ context.Context, in store.NewJob, work jobs.Work) (*store.Job, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.submitted = append(f.submitted, in)
	f.work = work
	return &store.Job{ID: uuid.New(), StorageID: in.StorageID, Kind: in.Kind, Status: store.JobPending}, nil
}

func (f *fakeRunner) Resubmit(_ context.Context, _, id uuid.UUID, work jobs.Work) error {
	if f.err != nil {
		return f.err
	}
	f.resubmitted = append(f.resubmitted, id)
	f.work = work
	return nil
}

type fakeAnalyzer struct {
	analysis *vision.Analysis
	err      error
	gotModel string
	gotMode  vision.Mode
	gotMime  string
	gotImage []byte
}

func (f *fakeAnalyzer) Analyze(_ context.Context, model string, mode vision.Mode, image []byte, mime string) (*vision.Analysis, error) {
	f.gotModel, f.gotMode, f.gotImage, f.gotMime = model, mode, image, mime
	return f.analysis, f.err
}

type fakeModels struct {
	status      string
	invalidated int
}

func (f *fakeModels) EffectiveModel(context.Context) (string, error) { return "gemini-test", nil }
func (f *fakeModels) Status(context.Context) string                  { return f.status }
func (f *fakeModels) Invalidate()                                    { f.invalidated++ }

type fakeMatcher struct{ results map[string]matching.Result }

func (f *fakeMatcher) MatchProductCandidates(_ context.Context, _ uuid.UUID, text string) (matching.Result, error) {
	if r, ok := f.results[text]; ok {
		return r, nil
	}
	return matching.Result{Status: matching.StatusNewItem}, nil
}

type fakeStore struct {
	tree    []store.Location
	expired map[uuid.UUID]string
	cleared []uuid.UUID
}

func (f *fakeStore) LocationTree(context.Context, uuid.UUID) ([]store.Location, error) {
	return f.tree, nil
}
func (f *fakeStore) ExpiredJobImages(context.Context, time.Time, int) (map[uuid.UUID]string, error) {
	return f.expired, nil
}
func (f *fakeStore) ClearJobImage(_ context.Context, id uuid.UUID) error {
	f.cleared = append(f.cleared, id)
	return nil
}

type fakePhotos struct{ files map[string][]byte }

func (f *fakePhotos) Save(name string, data []byte) error { f.files[name] = data; return nil }
func (f *fakePhotos) Read(name string) ([]byte, error) {
	if d, ok := f.files[name]; ok {
		return d, nil
	}
	return nil, os.ErrNotExist
}
func (f *fakePhotos) Remove(name string) error { delete(f.files, name); return nil }

type harness struct {
	svc      *Service
	runner   *fakeRunner
	analyzer *fakeAnalyzer
	models   *fakeModels
	matcher  *fakeMatcher
	store    *fakeStore
	photos   *fakePhotos
}

func newHarness() *harness {
	h := &harness{
		runner:   &fakeRunner{},
		analyzer: &fakeAnalyzer{},
		models:   &fakeModels{status: vision.StatusOK},
		matcher:  &fakeMatcher{results: map[string]matching.Result{}},
		store:    &fakeStore{},
		photos:   &fakePhotos{files: map[string][]byte{}},
	}
	h.svc = NewService(h.runner, h.analyzer, h.models, h.matcher, h.store, h.photos, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h
}

const photoName = "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.jpg"

func (h *harness) start(t *testing.T, kind store.JobKind, hint *uuid.UUID) (json.RawMessage, error) {
	t.Helper()
	_, err := h.svc.Start(context.Background(), Upload{
		StorageID: uuid.New(), Kind: kind, CreatedBy: uuid.New(), LocationHintID: hint,
		Filename: photoName, Image: []byte("stripped jpeg"),
	})
	require.NoError(t, err)
	require.NotNil(t, h.runner.work)
	return h.runner.work(context.Background())
}

// --- tests ---

func loc(name string, parent *uuid.UUID) store.Location {
	return store.Location{ID: uuid.New(), Name: name, ParentID: parent}
}

func TestPlaceResolvesAPathAgainstTheTree(t *testing.T) {
	t.Parallel()

	basement := loc("Basement", nil)
	right := loc("Right Shelf", &basement.ID)
	layer := loc("Layer 2", &right.ID)
	kitchen := loc("Kitchen", nil)
	// A same-named node under another parent must not be matched.
	strayLayer := loc("Layer 2", &kitchen.ID)
	idx := newTreeIndex([]store.Location{basement, right, layer, kitchen, strayLayer})
	hint := uuid.New()

	t.Run("full path preselects the leaf", func(t *testing.T) {
		p := idx.place([]string{"basement", " Right shelf ", "LAYER 2"}, &hint)
		require.NotNil(t, p.LocationID)
		assert.Equal(t, layer.ID, *p.LocationID)
		for _, seg := range p.Path {
			assert.False(t, seg.Proposed)
		}
	})

	t.Run("partial path proposes the rest and preselects nothing", func(t *testing.T) {
		p := idx.place([]string{"Basement", "Left Shelf", "Layer 2"}, &hint)
		assert.Nil(t, p.LocationID, "part of the path does not exist yet")
		require.Len(t, p.Path, 3)
		assert.Equal(t, basement.ID, *p.Path[0].LocationID)
		assert.True(t, p.Path[1].Proposed)
		assert.True(t, p.Path[2].Proposed, "nothing can exist under a proposed parent")
		assert.Nil(t, p.Path[2].LocationID, "the stray Layer 2 elsewhere is not this one")
	})

	t.Run("empty path falls back to the hint", func(t *testing.T) {
		p := idx.place(nil, &hint)
		assert.Equal(t, &hint, p.LocationID)
		assert.Empty(t, p.Path)
	})
}

func TestWorkBuildsAProposalFromTheAnalysis(t *testing.T) {
	t.Parallel()

	h := newHarness()
	pantry := loc("Pantry", nil)
	h.store.tree = []store.Location{pantry}

	beans := uuid.New()
	catalogID := uuid.New()
	category := "Food > Pasta"
	h.matcher.results["Beans 400g"] = matching.Result{Status: matching.StatusExactMatch,
		Product: &matching.LocalCandidate{ProductID: beans, Name: "Beans"}}
	h.matcher.results["Penne"] = matching.Result{Status: matching.StatusNewItem,
		Catalog: &matching.CatalogMatch{ID: catalogID, DisplayName: "Penne Rigate", CategoryPath: &category, ItemType: "long_shelf_life"}}
	h.analyzer.analysis = &vision.Analysis{Items: []vision.Item{
		{Label: "Beans 400g", Confidence: 0.9, Quantity: 3, ProposedLocationPath: []string{"Pantry"},
			BoundingBox: &vision.Box{X: 0.1, Y: 0.1, Width: 0.2, Height: 0.2}},
		{Label: "Penne", Confidence: 0.6, Quantity: 1, ProposedLocationPath: []string{}},
	}}

	payload, err := h.start(t, store.JobShelfIngestion, nil)
	require.NoError(t, err)

	assert.Equal(t, vision.ModeShelf, h.analyzer.gotMode)
	assert.Equal(t, "gemini-test", h.analyzer.gotModel)
	assert.Equal(t, "image/jpeg", h.analyzer.gotMime)
	assert.Equal(t, []byte("stripped jpeg"), h.analyzer.gotImage, "the stored photo is what gets analysed")

	assert.NotContains(t, string(payload), catalogID.String(), "a catalog row's id never reaches the browser")

	var proposal Proposal
	require.NoError(t, json.Unmarshal(payload, &proposal))
	require.Len(t, proposal.Rows, 2)

	first := proposal.Rows[0]
	assert.Equal(t, "0", first.RowID)
	assert.Equal(t, "exact_match", first.Match.Status)
	require.NotNil(t, first.Match.Product)
	assert.Equal(t, beans, first.Match.Product.ID)
	require.NotNil(t, first.Location.LocationID)
	assert.Equal(t, pantry.ID, *first.Location.LocationID)
	require.NotNil(t, first.BoundingBox)

	second := proposal.Rows[1]
	assert.Equal(t, "1", second.RowID)
	require.NotNil(t, second.Match.Catalog)
	assert.Equal(t, "Penne Rigate", second.Match.Catalog.DisplayName)
	assert.NotNil(t, second.Match.Candidates, "an empty list, not null, for the review UI")
}

// TestWorkFailuresAreWrittenForTheReviewer — the acceptance criterion that a
// malformed response fails the job with a retryable message, plus the other
// failures a reviewer can act on.
func TestWorkFailuresAreWrittenForTheReviewer(t *testing.T) {
	t.Parallel()

	t.Run("malformed response", func(t *testing.T) {
		h := newHarness()
		h.analyzer.err = vision.ErrMalformedResponse
		_, err := h.start(t, store.JobShelfIngestion, nil)

		var userErr *jobs.UserError
		require.ErrorAs(t, err, &userErr)
		assert.Equal(t, msgUnreadable, userErr.Message)
	})

	t.Run("model withdrawn", func(t *testing.T) {
		h := newHarness()
		h.analyzer.err = vision.ErrModelNotFound
		_, err := h.start(t, store.JobProductPhoto, nil)

		var userErr *jobs.UserError
		require.ErrorAs(t, err, &userErr)
		assert.Equal(t, msgModelUnavailable, userErr.Message)
		assert.Equal(t, 1, h.models.invalidated, "the availability cache is dropped so the next upload says 503")
	})

	t.Run("photo gone", func(t *testing.T) {
		h := newHarness()
		_, err := h.svc.Start(context.Background(), Upload{StorageID: uuid.New(), Kind: store.JobShelfIngestion, Filename: photoName, Image: []byte("x")})
		require.NoError(t, err)
		delete(h.photos.files, photoName)

		_, err = h.runner.work(context.Background())
		var userErr *jobs.UserError
		require.ErrorAs(t, err, &userErr)
		assert.Equal(t, msgPhotoMissing, userErr.Message)
	})

	t.Run("outage stays generic", func(t *testing.T) {
		h := newHarness()
		h.analyzer.err = errors.New("vision: generateContent returned 503")
		_, err := h.start(t, store.JobShelfIngestion, nil)

		var userErr *jobs.UserError
		assert.False(t, errors.As(err, &userErr), "provider detail is for the log, not the inbox")
	})
}

func TestStartSavesThePhotoBeforeTheJob(t *testing.T) {
	t.Parallel()

	h := newHarness()
	hint := uuid.New()
	job, err := h.svc.Start(context.Background(), Upload{
		StorageID: uuid.New(), Kind: store.JobProductPhoto, CreatedBy: uuid.New(),
		LocationHintID: &hint, Filename: photoName, Image: []byte("bytes"),
	})
	require.NoError(t, err)
	assert.Equal(t, store.JobPending, job.Status)

	assert.Contains(t, h.photos.files, photoName)
	require.Len(t, h.runner.submitted, 1)
	assert.Equal(t, photoName, *h.runner.submitted[0].ImageFilename)
	assert.Equal(t, &hint, h.runner.submitted[0].LocationHintID)

	failing := newHarness()
	failing.runner.err = store.ErrNotFound
	_, err = failing.svc.Start(context.Background(), Upload{StorageID: uuid.New(), Kind: store.JobShelfIngestion, Filename: photoName, Image: []byte("x")})
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.NotContains(t, failing.photos.files, photoName, "a photo no job references is removed again")

	_, err = h.svc.Start(context.Background(), Upload{StorageID: uuid.New(), Kind: store.JobConsumptionPhoto, Filename: photoName})
	assert.ErrorIs(t, err, ErrUnsupportedKind)
}

func TestAvailableReflectsTheModelChecker(t *testing.T) {
	t.Parallel()

	h := newHarness()
	model, ok := h.svc.Available(context.Background())
	assert.True(t, ok)
	assert.Equal(t, "gemini-test", model)

	h.models.status = vision.StatusModelUnavailable
	_, ok = h.svc.Available(context.Background())
	assert.False(t, ok)
}

func TestSweepImagesRemovesExpiredPhotos(t *testing.T) {
	t.Parallel()

	h := newHarness()
	id := uuid.New()
	h.photos.files[photoName] = []byte("old")
	h.store.expired = map[uuid.UUID]string{id: photoName}

	n, err := h.svc.SweepImages(context.Background(), time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NotContains(t, h.photos.files, photoName)
	assert.Equal(t, []uuid.UUID{id}, h.store.cleared)
}

// TestReanalyzeRunsTheJobsOwnAnalysisAgain — "Analyze again" re-runs the same
// photo in the job's own mode, keeps its location hint, and builds a fresh
// proposal; a job this service did not create, or one with no photo, is
// refused without touching the runner.
func TestReanalyzeRunsTheJobsOwnAnalysisAgain(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.photos.files[photoName] = []byte("stripped jpeg")
	h.analyzer.analysis = &vision.Analysis{Items: []vision.Item{{Label: "Rolled Oats", Confidence: 0.8, Quantity: 1}}}

	photo := photoName
	hint := uuid.New()
	job := &store.Job{ID: uuid.New(), StorageID: uuid.New(), Kind: store.JobProductPhoto, Status: store.JobFailed,
		ImageFilename: &photo, LocationHintID: &hint}

	require.NoError(t, h.svc.Reanalyze(context.Background(), job))
	assert.Equal(t, []uuid.UUID{job.ID}, h.runner.resubmitted)
	assert.Empty(t, h.runner.submitted, "the same job again, not a new one")

	payload, err := h.runner.work(context.Background())
	require.NoError(t, err)
	assert.Equal(t, vision.ModeProduct, h.analyzer.gotMode, "the job's own kind decides the prompt")
	assert.Equal(t, []byte("stripped jpeg"), h.analyzer.gotImage, "the photo already stored is what gets analysed")

	var proposal Proposal
	require.NoError(t, json.Unmarshal(payload, &proposal))
	require.Len(t, proposal.Rows, 1)
	assert.Equal(t, "Rolled Oats", proposal.Rows[0].Label)
	require.NotNil(t, proposal.LocationHintID)
	assert.Equal(t, hint, *proposal.LocationHintID, "the upload's shelf still scopes the new proposal")

	other := newHarness()
	consumption := &store.Job{ID: uuid.New(), Kind: store.JobConsumptionPhoto, Status: store.JobDone, ImageFilename: &photo}
	assert.ErrorIs(t, other.svc.Reanalyze(context.Background(), consumption), ErrUnsupportedKind)
	noPhoto := &store.Job{ID: uuid.New(), Kind: store.JobShelfIngestion, Status: store.JobDone}
	assert.ErrorIs(t, other.svc.Reanalyze(context.Background(), noPhoto), store.ErrConflict)
	assert.Empty(t, other.runner.resubmitted)

	other.runner.err = store.ErrConflict
	assert.ErrorIs(t, other.svc.Reanalyze(context.Background(), job), store.ErrConflict, "the store's refusal reaches the caller")
}
