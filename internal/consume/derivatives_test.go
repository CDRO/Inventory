package consume

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/vision"
)

type fakeDerivatives struct {
	mu        sync.Mutex
	err       error
	photoSets []string
	rowSets   []map[string]images.Box
}

func (f *fakeDerivatives) EnsurePhotoSet(_ context.Context, filename string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.photoSets = append(f.photoSets, filename)
	return f.err
}

func (f *fakeDerivatives) ReplaceRowSet(_ context.Context, _ string, _ []byte, boxes map[string]images.Box) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowSets = append(f.rowSets, boxes)
	return f.err
}

// TestWorkMakesThePhotosThumbnailsAndEachRowsCrop — a consumption photo's
// review crops its rows exactly as a shelf photo's does
// (docs/specs/43-image-derivatives.md).
func TestWorkMakesThePhotosThumbnailsAndEachRowsCrop(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := &fakeDerivatives{}
	h.svc.WithDerivatives(d)
	h.analyzer.analysis = &vision.Analysis{Items: []vision.Item{
		{Label: "Empty Bean Can", Quantity: 1, BoundingBox: &vision.Box{X: 0.1, Y: 0.1, Width: 0.2, Height: 0.2}},
		{Label: "Unlabeled Jar", Quantity: 1},
	}}

	payload, err := h.start(t)
	require.NoError(t, err)

	assert.Equal(t, []string{photoName}, d.photoSets)
	require.Len(t, d.rowSets, 1)
	assert.Equal(t, map[string]images.Box{"0": {X: 0.1, Y: 0.1, Width: 0.2, Height: 0.2}}, d.rowSets[0])

	var proposal Proposal
	require.NoError(t, json.Unmarshal(payload, &proposal))
	assert.Len(t, proposal.Rows, 2)
}

func TestDerivativeFailuresNeverFailTheJob(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.svc.WithDerivatives(&fakeDerivatives{err: errors.New("cache volume full")})
	h.analyzer.analysis = &vision.Analysis{Items: []vision.Item{{Label: "Empty Bean Can", Quantity: 1}}}

	payload, err := h.start(t)
	require.NoError(t, err)
	assert.Contains(t, string(payload), "Empty Bean Can")
}
