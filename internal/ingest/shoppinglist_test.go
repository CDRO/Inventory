package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/jobs"
	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// docs/specs/07-shopping-list-reconciliation.md's photo ingestion, which had
// never been built: upload, background job, the model reads the lines, and
// "processing continues identically to the text path".

// startList runs one shopping-list photo job and returns its payload.
func (h *harness) startList(t *testing.T) (json.RawMessage, error) {
	t.Helper()
	_, err := h.svc.StartShoppingList(context.Background(), Upload{
		StorageID: uuid.New(), Kind: store.JobShoppingListPhoto, CreatedBy: uuid.New(),
		Filename: photoName, Image: []byte("stripped jpeg"),
	})
	require.NoError(t, err)
	require.NotNil(t, h.runner.work)
	return h.runner.work(context.Background())
}

func TestPhotographedListReadsAsAListAndBecomesOne(t *testing.T) {
	t.Parallel()

	h := newHarness()
	existing := uuid.New()
	h.matcher.results["milk"] = matching.Result{
		Status:  matching.StatusExactMatch,
		Product: &matching.LocalCandidate{ProductID: existing, Name: "Milk"},
	}
	h.analyzer.analysis = &vision.Analysis{
		LooksLikeShoppingList: true,
		ShoppingListLines:     []string{"milk", "eggs x2"},
	}

	payload, err := h.startList(t)
	require.NoError(t, err)

	// The photo is read as a list from the start, not as a shelf that might
	// mention one (docs/specs/41-mixed-photo-classification.md).
	assert.Equal(t, vision.ModeShoppingList, h.analyzer.gotMode)
	require.Len(t, h.runner.submitted, 1)
	assert.Equal(t, store.JobShoppingListPhoto, h.runner.submitted[0].Kind)

	// Each line went through the shared matching service, with the quantity
	// parsed off exactly as a typed list's is: "eggs x2" matches on "eggs".
	assert.Equal(t, store.SourcePhoto, h.store.listSource)
	require.Len(t, h.store.listCreated, 2)
	assert.Equal(t, "milk", h.store.listCreated[0].RawText)
	assert.Equal(t, store.ItemExactMatch, h.store.listCreated[0].Status)
	require.NotNil(t, h.store.listCreated[0].MatchedProductID)
	assert.Equal(t, existing, *h.store.listCreated[0].MatchedProductID)
	assert.Equal(t, "eggs x2", h.store.listCreated[1].RawText, "the line is stored whole")

	// The payload names the list, which is what the resolution screen opens.
	var out ShoppingListPayload
	require.NoError(t, json.Unmarshal(payload, &out))
	assert.Equal(t, h.store.listID, out.ShoppingListID)
}

// An unreadable photo is a failed job with something a person can act on, not
// an empty shopping list they would have to find and discard.
func TestAPhotoWithNoLinesFailsTheJobReadably(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.analyzer.analysis = &vision.Analysis{ShoppingListLines: []string{}}

	_, err := h.startList(t)

	var userErr *jobs.UserError
	require.ErrorAs(t, err, &userErr)
	assert.Contains(t, userErr.Message, "clearer photo")
	assert.Empty(t, h.store.listCreated, "nothing is written for a photo that read as nothing")
}

func TestAListPhotoReportsTheSameFailuresEveryOtherPhotoDoes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		analyzeErr error
		want       string
		invalidate bool
	}{
		"model withdrawn": {vision.ErrModelNotFound, msgModelUnavailable, true},
		"unreadable":      {vision.ErrMalformedResponse, msgUnreadable, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness()
			h.analyzer.err = tc.analyzeErr

			_, err := h.startList(t)

			var userErr *jobs.UserError
			require.ErrorAs(t, err, &userErr)
			assert.Equal(t, tc.want, userErr.Message)
			if tc.invalidate {
				assert.Equal(t, 1, h.models.invalidated, "the cached model list is dropped so /healthz flips")
			}
		})
	}
}

func TestAListPhotoWhoseFileVanishedSaysSo(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.analyzer.analysis = &vision.Analysis{ShoppingListLines: []string{"milk"}}
	_, err := h.svc.StartShoppingList(context.Background(), Upload{
		StorageID: uuid.New(), Kind: store.JobShoppingListPhoto, CreatedBy: uuid.New(),
		Filename: photoName, Image: []byte("stripped jpeg"),
	})
	require.NoError(t, err)
	require.NoError(t, h.photos.Remove(photoName))

	_, err = h.runner.work(context.Background())

	var userErr *jobs.UserError
	require.ErrorAs(t, err, &userErr)
	assert.Equal(t, msgPhotoMissing, userErr.Message)
}

// A failed write must not leave the photo behind as an orphan, the same rule
// Start follows: nothing would ever reference it.
func TestAListPhotoIsRemovedWhenItsJobIsNeverCreated(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.runner.err = errors.New("no job for you")

	_, err := h.svc.StartShoppingList(context.Background(), Upload{
		StorageID: uuid.New(), Kind: store.JobShoppingListPhoto, CreatedBy: uuid.New(),
		Filename: photoName, Image: []byte("stripped jpeg"),
	})

	require.Error(t, err)
	assert.Empty(t, h.photos.files)
}

func TestStartShoppingListRefusesAnotherKind(t *testing.T) {
	t.Parallel()

	h := newHarness()
	_, err := h.svc.StartShoppingList(context.Background(), Upload{
		StorageID: uuid.New(), Kind: store.JobShelfIngestion, CreatedBy: uuid.New(),
		Filename: photoName, Image: []byte("stripped jpeg"),
	})

	require.ErrorIs(t, err, ErrUnsupportedKind)
	assert.Empty(t, h.photos.files, "a refused upload writes no photo")
}

// A shopping-list photo is not analysed again: its job does not end in a
// proposal waiting for review, it ends in a real list, so repeating it would
// write a second one next to the first.
func TestAListPhotoIsNotReanalyzable(t *testing.T) {
	t.Parallel()

	h := newHarness()
	err := h.svc.Reanalyze(context.Background(), &store.Job{
		ID: uuid.New(), Kind: store.JobShoppingListPhoto, ImageFilename: strPtr(photoName),
	})

	require.ErrorIs(t, err, ErrUnsupportedKind)
	assert.Empty(t, h.runner.resubmitted)
}

func strPtr(s string) *string { return &s }
