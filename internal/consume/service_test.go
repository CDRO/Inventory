package consume

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

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

// fakeMatcher implements only MatchLocalProduct — the same interface
// satisfaction that proves the service can never reach stage 2 or 3, since
// there is no other method on Matcher for it to call.
type fakeMatcher struct {
	results map[string]matching.Result
	calls   []string
}

func (f *fakeMatcher) MatchLocalProduct(_ context.Context, _ uuid.UUID, text string) (matching.Result, error) {
	f.calls = append(f.calls, text)
	if r, ok := f.results[text]; ok {
		return r, nil
	}
	return matching.Result{Status: matching.StatusNewItem}, nil
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
	photos   *fakePhotos
}

func newHarness() *harness {
	h := &harness{
		runner:   &fakeRunner{},
		analyzer: &fakeAnalyzer{},
		models:   &fakeModels{status: vision.StatusOK},
		matcher:  &fakeMatcher{results: map[string]matching.Result{}},
		photos:   &fakePhotos{files: map[string][]byte{}},
	}
	h.svc = NewService(h.runner, h.analyzer, h.models, h.matcher, h.photos, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h
}

const photoName = "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.jpg"

func (h *harness) start(t *testing.T) (json.RawMessage, error) {
	t.Helper()
	_, err := h.svc.Start(context.Background(), Upload{
		StorageID: uuid.New(), CreatedBy: uuid.New(), Filename: photoName, Image: []byte("stripped jpeg"),
	})
	require.NoError(t, err)
	require.NotNil(t, h.runner.work)
	return h.runner.work(context.Background())
}

// --- tests ---

// TestWorkBuildsAProposalFromTheAnalysis — the model is asked in
// ModeConsumption, each label is resolved through MatchLocalProduct only
// (never MatchProductCandidates, which this fake does not even implement),
// and a catalog-shaped match never appears — consumption has nothing to put
// one in.
func TestWorkBuildsAProposalFromTheAnalysis(t *testing.T) {
	t.Parallel()

	h := newHarness()
	beans := uuid.New()
	h.matcher.results["Empty Bean Can"] = matching.Result{Status: matching.StatusExactMatch,
		Product: &matching.LocalCandidate{ProductID: beans, Name: "Beans"}}
	h.analyzer.analysis = &vision.Analysis{Items: []vision.Item{
		{Label: "Empty Bean Can", Confidence: 0.9, Quantity: 1,
			BoundingBox: &vision.Box{X: 0.1, Y: 0.1, Width: 0.2, Height: 0.2}},
		{Label: "Unlabeled Jar", Confidence: 0.4, Quantity: 1},
	}}

	payload, err := h.start(t)
	require.NoError(t, err)

	assert.Equal(t, vision.ModeConsumption, h.analyzer.gotMode)
	assert.Equal(t, "gemini-test", h.analyzer.gotModel)
	assert.Equal(t, "image/jpeg", h.analyzer.gotMime)
	assert.Equal(t, []byte("stripped jpeg"), h.analyzer.gotImage, "the stored photo is what gets analysed")
	assert.Equal(t, []string{"Empty Bean Can", "Unlabeled Jar"}, h.matcher.calls, "every row is matched, stage 1 only")

	var proposal Proposal
	require.NoError(t, json.Unmarshal(payload, &proposal))
	require.Len(t, proposal.Rows, 2)

	first := proposal.Rows[0]
	assert.Equal(t, "0", first.RowID)
	assert.Equal(t, "exact_match", first.Match.Status)
	require.NotNil(t, first.Match.Product)
	assert.Equal(t, beans, first.Match.Product.ID)
	require.NotNil(t, first.BoundingBox, "consumption keeps the crop, like shelf ingestion")

	second := proposal.Rows[1]
	assert.Equal(t, "1", second.RowID)
	assert.Equal(t, "new_item", second.Match.Status, "unrecognized: nothing here ever tries the catalog")
	assert.Nil(t, second.Match.Product)
	assert.NotNil(t, second.Match.Candidates, "an empty list, not null, for the review UI")
}

// TestWorkFailuresAreWrittenForTheReviewer mirrors internal/ingest's
// equivalent test — the two packages share the same failure shape by design.
func TestWorkFailuresAreWrittenForTheReviewer(t *testing.T) {
	t.Parallel()

	t.Run("malformed response", func(t *testing.T) {
		h := newHarness()
		h.analyzer.err = vision.ErrMalformedResponse
		_, err := h.start(t)

		var userErr *jobs.UserError
		require.ErrorAs(t, err, &userErr)
		assert.Equal(t, msgUnreadable, userErr.Message)
	})

	t.Run("model withdrawn", func(t *testing.T) {
		h := newHarness()
		h.analyzer.err = vision.ErrModelNotFound
		_, err := h.start(t)

		var userErr *jobs.UserError
		require.ErrorAs(t, err, &userErr)
		assert.Equal(t, msgModelUnavailable, userErr.Message)
		assert.Equal(t, 1, h.models.invalidated, "the availability cache is dropped so the next upload says 503")
	})

	t.Run("photo gone", func(t *testing.T) {
		h := newHarness()
		_, err := h.svc.Start(context.Background(), Upload{StorageID: uuid.New(), Filename: photoName, Image: []byte("x")})
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
		_, err := h.start(t)

		var userErr *jobs.UserError
		assert.False(t, errors.As(err, &userErr), "provider detail is for the log, not the inbox")
	})
}

func TestStartSavesThePhotoBeforeTheJob(t *testing.T) {
	t.Parallel()

	h := newHarness()
	job, err := h.svc.Start(context.Background(), Upload{
		StorageID: uuid.New(), CreatedBy: uuid.New(), Filename: photoName, Image: []byte("bytes"),
	})
	require.NoError(t, err)
	assert.Equal(t, store.JobPending, job.Status)

	assert.Contains(t, h.photos.files, photoName)
	require.Len(t, h.runner.submitted, 1)
	assert.Equal(t, store.JobConsumptionPhoto, h.runner.submitted[0].Kind)
	assert.Equal(t, photoName, *h.runner.submitted[0].ImageFilename)
	assert.Nil(t, h.runner.submitted[0].LocationHintID, "consumption places nothing, so there is no hint to carry")

	failing := newHarness()
	failing.runner.err = store.ErrNotFound
	_, err = failing.svc.Start(context.Background(), Upload{StorageID: uuid.New(), Filename: photoName, Image: []byte("x")})
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.NotContains(t, failing.photos.files, photoName, "a photo no job references is removed again")
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

// TestReanalyzeRunsTheConsumptionAnalysisAgain — "Analyze again" re-runs the
// same photo in ModeConsumption and builds a fresh proposal; a job this
// service did not create, or one with no photo, is refused without touching
// the runner.
func TestReanalyzeRunsTheConsumptionAnalysisAgain(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.photos.files[photoName] = []byte("stripped jpeg")
	h.analyzer.analysis = &vision.Analysis{Items: []vision.Item{{Label: "Empty Bean Can", Confidence: 0.9, Quantity: 2}}}

	photo := photoName
	job := &store.Job{ID: uuid.New(), StorageID: uuid.New(), Kind: store.JobConsumptionPhoto, Status: store.JobDone, ImageFilename: &photo}

	require.NoError(t, h.svc.Reanalyze(context.Background(), job))
	assert.Equal(t, []uuid.UUID{job.ID}, h.runner.resubmitted)
	assert.Empty(t, h.runner.submitted, "the same job again, not a new one")

	payload, err := h.runner.work(context.Background())
	require.NoError(t, err)
	assert.Equal(t, vision.ModeConsumption, h.analyzer.gotMode)
	assert.Equal(t, []byte("stripped jpeg"), h.analyzer.gotImage, "the photo already stored is what gets analysed")

	var proposal Proposal
	require.NoError(t, json.Unmarshal(payload, &proposal))
	require.Len(t, proposal.Rows, 1)
	assert.Equal(t, 2, proposal.Rows[0].Quantity)

	other := newHarness()
	shelf := &store.Job{ID: uuid.New(), Kind: store.JobShelfIngestion, Status: store.JobDone, ImageFilename: &photo}
	assert.ErrorIs(t, other.svc.Reanalyze(context.Background(), shelf), ErrUnsupportedKind)
	noPhoto := &store.Job{ID: uuid.New(), Kind: store.JobConsumptionPhoto, Status: store.JobDone}
	assert.ErrorIs(t, other.svc.Reanalyze(context.Background(), noPhoto), store.ErrConflict)
	assert.Empty(t, other.runner.resubmitted)
}
