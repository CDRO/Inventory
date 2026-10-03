package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// fakeDerivatives records what a job asked to have made. Guarded, because
// the photo set is made on a goroutine beside the model call.
type fakeDerivatives struct {
	mu         sync.Mutex
	err        error
	photoSets  []string
	photoBytes [][]byte
	rowSets    []map[string]images.Box
}

func (f *fakeDerivatives) EnsurePhotoSet(_ context.Context, filename string, photo []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.photoSets = append(f.photoSets, filename)
	f.photoBytes = append(f.photoBytes, photo)
	return f.err
}

func (f *fakeDerivatives) ReplaceRowSet(_ context.Context, _ string, _ []byte, boxes map[string]images.Box) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowSets = append(f.rowSets, boxes)
	return f.err
}

// TestWorkMakesThePhotosThumbnailsAndEachRowsCrop — the eager path of
// docs/specs/43-image-derivatives.md: the whole-picture set from the photo
// the job read, and one crop per row that has a box, keyed by row id.
func TestWorkMakesThePhotosThumbnailsAndEachRowsCrop(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := &fakeDerivatives{}
	h.svc.WithDerivatives(d)
	h.analyzer.analysis = &vision.Analysis{Items: []vision.Item{
		{Label: "Beans", Quantity: 1, BoundingBox: &vision.Box{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}},
		{Label: "Penne", Quantity: 1},
		{Label: "Rice", Quantity: 2, BoundingBox: &vision.Box{X: 0.5, Y: 0.5, Width: 0.5, Height: 0.5}},
	}}

	payload, err := h.start(t, store.JobShelfIngestion, nil)
	require.NoError(t, err)

	require.Equal(t, []string{photoName}, d.photoSets)
	assert.Equal(t, []byte("stripped jpeg"), d.photoBytes[0], "made from the photo the job already read")
	require.Len(t, d.rowSets, 1)
	assert.Equal(t, map[string]images.Box{
		"0": {X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4},
		"2": {X: 0.5, Y: 0.5, Width: 0.5, Height: 0.5},
	}, d.rowSets[0], "only rows with a box, under the row id the review will ask by")

	var proposal Proposal
	require.NoError(t, json.Unmarshal(payload, &proposal))
	assert.Len(t, proposal.Rows, 3)
}

// TestDerivativeFailuresNeverFailTheJob — a thumbnail is never worth a
// proposal: the job finishes exactly as it would have without derivatives,
// and the serving route makes the missing pictures on request.
func TestDerivativeFailuresNeverFailTheJob(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.svc.WithDerivatives(&fakeDerivatives{err: errors.New("cache volume full")})
	h.analyzer.analysis = &vision.Analysis{Items: []vision.Item{
		{Label: "Beans", Quantity: 1, BoundingBox: &vision.Box{X: 0.1, Y: 0.1, Width: 0.2, Height: 0.2}},
	}}

	payload, err := h.start(t, store.JobShelfIngestion, nil)
	require.NoError(t, err)

	var proposal Proposal
	require.NoError(t, json.Unmarshal(payload, &proposal))
	assert.Len(t, proposal.Rows, 1)
}

// TestAFailedAnalysisStillMakesThePhotosThumbnails — the inbox shows a failed
// job's card with its photo too, and the set was started before the model
// was asked.
func TestAFailedAnalysisStillMakesThePhotosThumbnails(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := &fakeDerivatives{}
	h.svc.WithDerivatives(d)
	h.analyzer.err = vision.ErrMalformedResponse

	_, err := h.start(t, store.JobShelfIngestion, nil)
	require.Error(t, err)

	assert.Equal(t, []string{photoName}, d.photoSets)
	assert.Empty(t, d.rowSets, "no proposal, no crops")
}

// TestShoppingListWorkMakesOnlyThePhotoSet — a photographed list has an inbox
// card but no boxes to cut.
func TestShoppingListWorkMakesOnlyThePhotoSet(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := &fakeDerivatives{}
	h.svc.WithDerivatives(d)
	h.matcher.results["Milk"] = matching.Result{Status: matching.StatusNewItem}
	h.analyzer.analysis = &vision.Analysis{ShoppingListLines: []string{"Milk"}}

	_, err := h.svc.StartShoppingList(context.Background(), Upload{
		StorageID: uuid.New(), Kind: store.JobShoppingListPhoto, CreatedBy: uuid.New(),
		Filename: photoName, Image: []byte("stripped jpeg"),
	})
	require.NoError(t, err)
	require.NotNil(t, h.runner.work)
	_, err = h.runner.work(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []string{photoName}, d.photoSets)
	assert.Empty(t, d.rowSets)
}
