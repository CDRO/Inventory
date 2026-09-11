package matching_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/matching"
)

// recordingStore counts what each stage asked for, so a test can assert on the
// calls that did NOT happen. That is the interesting half here: the spec's
// acceptance criterion is that a line already described by the catalog "never
// triggers a Gemini or SerpAPI request", and the enforcement is that matching
// stops before anything decides to make one.
type recordingStore struct {
	local      []matching.LocalCandidate
	localErr   error
	localCalls int

	catalog      *matching.CatalogMatch
	catalogErr   error
	catalogCalls int

	variants      []matching.CatalogVariant
	variantsErr   error
	variantsCalls int

	lastMinSimilarity float64
	lastQuery         string
}

func (r *recordingStore) SimilarProducts(_ context.Context, _ uuid.UUID, text string, minSimilarity float64, _ int) ([]matching.LocalCandidate, error) {
	r.localCalls++
	r.lastQuery = text
	r.lastMinSimilarity = minSimilarity
	return r.local, r.localErr
}

func (r *recordingStore) SimilarCatalogProduct(_ context.Context, _ string, _ float64) (*matching.CatalogMatch, error) {
	r.catalogCalls++
	return r.catalog, r.catalogErr
}

func (r *recordingStore) CatalogVariantsOf(_ context.Context, _ uuid.UUID, _ string, _ int) ([]matching.CatalogVariant, error) {
	r.variantsCalls++
	return r.variants, r.variantsErr
}

func match(t *testing.T, store *recordingStore, text string) matching.Result {
	t.Helper()

	result, err := matching.New(store).MatchProductCandidates(context.Background(), uuid.New(), text)
	require.NoError(t, err)
	return result
}

// TestStage1HitNeverReachesTheCatalog is the cheap-first rule at its first
// boundary. A storage that already has the product must not cause a catalog
// query, let alone anything beyond it.
func TestStage1HitNeverReachesTheCatalog(t *testing.T) {
	t.Parallel()

	store := &recordingStore{
		local: []matching.LocalCandidate{{ProductID: uuid.New(), Name: "Whole Milk", Similarity: 0.92}},
	}

	result := match(t, store, "whole milk")

	assert.Equal(t, matching.StatusExactMatch, result.Status)
	require.NotNil(t, result.Product)
	assert.Equal(t, "Whole Milk", result.Product.Name)

	assert.Equal(t, 1, store.localCalls)
	assert.Zero(t, store.catalogCalls, "a local hit must not cost a catalog lookup")
	assert.Zero(t, store.variantsCalls)
	assert.False(t, result.NeedsExternalLookup(), "and certainly not an external call")
}

// TestCatalogHitNeedsNoExternalLookup is the spec's acceptance criterion
// stated directly: "A line whose product is already described in
// catalog_products never triggers a Gemini or SerpAPI request."
func TestCatalogHitNeedsNoExternalLookup(t *testing.T) {
	t.Parallel()

	store := &recordingStore{
		local:    nil,
		catalog:  &matching.CatalogMatch{ID: uuid.New(), DisplayName: "Tomatoes"},
		variants: []matching.CatalogVariant{{ID: uuid.New(), DisplayName: "Cherry Tomatoes"}},
	}

	result := match(t, store, "thomatoes, c.")

	assert.Equal(t, matching.StatusNewItem, result.Status)
	require.NotNil(t, result.Catalog)
	assert.Equal(t, "Tomatoes", result.Catalog.DisplayName)
	require.Len(t, result.Catalog.Variants, 1)
	assert.Equal(t, "Cherry Tomatoes", result.Catalog.Variants[0].DisplayName,
		"the variant is what makes an abbreviated line usable at all")

	assert.False(t, result.NeedsExternalLookup(),
		"a catalog hit is pre-filled data; spending an API call on it is the exact waste the staging exists to prevent")
}

// TestOnlyAFullMissAsksForAnExternalLookup pins the other side of the same
// rule: the expensive path is available, but only once both cheap stages have
// genuinely produced nothing.
func TestOnlyAFullMissAsksForAnExternalLookup(t *testing.T) {
	t.Parallel()

	store := &recordingStore{local: nil, catalog: nil}

	result := match(t, store, "obscure imported thing")

	assert.Equal(t, matching.StatusNewItem, result.Status)
	assert.Nil(t, result.Catalog)
	assert.True(t, result.NeedsExternalLookup())
	assert.Equal(t, 1, store.localCalls)
	assert.Equal(t, 1, store.catalogCalls)
	assert.Zero(t, store.variantsCalls, "no hit means no variants to fetch")
}

// TestAmbiguousWhenCandidatesAreTooClose — picking for the user here is how
// "cream" quietly becomes "sour cream" in someone's inventory.
func TestAmbiguousWhenCandidatesAreTooClose(t *testing.T) {
	t.Parallel()

	store := &recordingStore{
		local: []matching.LocalCandidate{
			{ProductID: uuid.New(), Name: "Cream", Similarity: 0.81},
			{ProductID: uuid.New(), Name: "Sour Cream", Similarity: 0.79},
		},
	}

	result := match(t, store, "cream")

	assert.Equal(t, matching.StatusAmbiguous, result.Status,
		"both are above the exact threshold, but they are 0.02 apart — a person decides")
	assert.Nil(t, result.Product)
	assert.Len(t, result.Candidates, 2)
	assert.Zero(t, store.catalogCalls, "an ambiguous local result is still a local result")
}

// TestAmbiguousWhenTheBestIsOnlyPlausible covers the other route into the same
// state: one candidate, but not a convincing one.
func TestAmbiguousWhenTheBestIsOnlyPlausible(t *testing.T) {
	t.Parallel()

	store := &recordingStore{
		local: []matching.LocalCandidate{{ProductID: uuid.New(), Name: "Oat Milk", Similarity: 0.42}},
	}

	result := match(t, store, "milk")

	assert.Equal(t, matching.StatusAmbiguous, result.Status)
	require.Len(t, result.Candidates, 1)
	assert.Equal(t, "Oat Milk", result.Candidates[0].Name)
}

// TestAClearWinnerAboveTheThresholdIsExact guards the margin rule from the
// other direction: a strong best with a distant runner-up must not be demoted
// to a question.
func TestAClearWinnerAboveTheThresholdIsExact(t *testing.T) {
	t.Parallel()

	store := &recordingStore{
		local: []matching.LocalCandidate{
			{ProductID: uuid.New(), Name: "Whole Milk", Similarity: 0.95},
			{ProductID: uuid.New(), Name: "Oat Milk", Similarity: 0.40},
		},
	}

	result := match(t, store, "whole milk")

	assert.Equal(t, matching.StatusExactMatch, result.Status)
}

// TestTheStoreIsAskedForTheDocumentedFloor keeps the named constants honest:
// they exist so tuning is a one-line change, which only holds if the query
// actually uses them.
func TestTheStoreIsAskedForTheDocumentedFloor(t *testing.T) {
	t.Parallel()

	store := &recordingStore{}
	match(t, store, "  Cherry   TOMATOES ")

	assert.InDelta(t, matching.AmbiguousThreshold, store.lastMinSimilarity, 0.0001)
	assert.Equal(t, "cherry tomatoes", store.lastQuery,
		"the query must be normalized the same way catalog rows are stored")
}

func TestEmptyTextIsAnError(t *testing.T) {
	t.Parallel()

	_, err := matching.New(&recordingStore{}).MatchProductCandidates(context.Background(), uuid.New(), "   ")

	require.Error(t, err, "an empty line is a caller bug; answering new_item would hide it")
}

func TestStoreFailuresAreReportedNotSwallowed(t *testing.T) {
	t.Parallel()

	t.Run("local stage", func(t *testing.T) {
		t.Parallel()

		store := &recordingStore{localErr: errors.New("boom")}
		_, err := matching.New(store).MatchProductCandidates(context.Background(), uuid.New(), "milk")

		require.Error(t, err)
		assert.Zero(t, store.catalogCalls, "a failed stage must not fall through to the next one")
	})

	t.Run("catalog stage", func(t *testing.T) {
		t.Parallel()

		store := &recordingStore{catalogErr: errors.New("boom")}
		_, err := matching.New(store).MatchProductCandidates(context.Background(), uuid.New(), "milk")

		require.Error(t, err,
			"a catalog outage must not be reported as 'nothing known', which would spend an API call on a product we do know")
	})
}
