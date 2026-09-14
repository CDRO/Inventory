package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// fakeCategories is the same shape as fakeLocations' read half — this
// endpoint is read-only, so there is nothing to write.
type fakeCategories struct {
	tree          []store.Category
	treeErr       error
	lastStorageID uuid.UUID
}

func (f *fakeCategories) CategoryTree(_ context.Context, storageID uuid.UUID) ([]store.Category, error) {
	f.lastStorageID = storageID
	return f.tree, f.treeErr
}

// TestCategoryTreeNestsChildrenWhateverTheRowOrder is CategoryTree's
// counterpart to TestLocationTreeNestsChildrenWhateverTheRowOrder
// (locations_test.go): CategoryTree orders by created_at, which stops
// putting parents first the moment an old node is re-parented under a newer
// one, and nestCategories has to handle that the same way nestLocations does.
func TestCategoryTreeNestsChildrenWhateverTheRowOrder(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	parent := uuid.New()
	child := uuid.New()

	f.categories.tree = []store.Category{
		{ID: child, StorageID: f.storageID, ParentID: &parent, Name: "Dairy"},
		{ID: parent, StorageID: f.storageID, Name: "Food"},
	}

	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Items []struct {
			ID       uuid.UUID `json:"id"`
			Name     string    `json:"name"`
			Children []struct {
				ID   uuid.UUID `json:"id"`
				Name string    `json:"name"`
			} `json:"children"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Len(t, body.Items, 1, "the child must not surface as a second root")
	assert.Equal(t, parent, body.Items[0].ID)
	require.Len(t, body.Items[0].Children, 1)
	assert.Equal(t, child, body.Items[0].Children[0].ID)
}

// TestCategoryTreeResponseNeverCarriesStorageID asserts on the wire format,
// the same reason locationNode's counterpart test does: the field must not
// exist for a client to read, not merely go unused.
func TestCategoryTreeResponseNeverCarriesStorageID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.categories.tree = []store.Category{{ID: uuid.New(), StorageID: f.storageID, Name: "Pantry"}}

	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "storage_id")
}

// TestCategoryTreeIsStorageScoped confirms the handler reads storageID from
// the request context (the RequireStorageMember-verified value), not from
// anywhere a caller could supply directly.
func TestCategoryTreeIsStorageScoped(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.categories.tree = []store.Category{{ID: uuid.New(), StorageID: f.storageID, Name: "Pantry"}}

	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, f.storageID, f.categories.lastStorageID)
}

// TestCategoryTreeEmptyIsAnEmptyListNotNull matches every other collection
// endpoint in this package: "items": [] on nothing found, never null.
func TestCategoryTreeEmptyIsAnEmptyListNotNull(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"items":[]`)
}
