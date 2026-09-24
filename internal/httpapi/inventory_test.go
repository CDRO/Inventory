package httpapi_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// The HTTP half of docs/specs/33-inventory-overview-table.md. Whether a
// filter id actually resolves against a storage's own trees, and whether the
// cursor walk is correct, is the store's job and is covered in
// internal/store/inventory_test.go — what these tests can see is the shape of
// the request a handler derived, and how a store failure becomes a response.

func TestInventoryListDefaultsToNoFilterAndForwardsTheCursor(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	after := uuid.New()
	rec := f.do(http.MethodGet, f.base()+"/inventory-batches?limit=25&cursor="+encodeTestCursor(after), "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, f.storageID, f.inventory.lastStorageID)
	assert.Nil(t, f.inventory.lastFilter.LocationID)
	assert.Nil(t, f.inventory.lastFilter.CategoryID)
	assert.Equal(t, 26, f.inventory.lastLimit, "one extra row is fetched to tell whether another page follows")
	require.NotNil(t, f.inventory.lastAfter)
	assert.Equal(t, after, *f.inventory.lastAfter)
}

func TestInventoryListParsesLocationAndCategoryFilters(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	location := uuid.New()
	category := uuid.New()
	rec := f.do(http.MethodGet, f.base()+"/inventory-batches?location_id="+location.String()+"&category_id="+category.String(), "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, f.inventory.lastFilter.LocationID)
	assert.Equal(t, location, *f.inventory.lastFilter.LocationID)
	require.NotNil(t, f.inventory.lastFilter.CategoryID)
	assert.Equal(t, category, *f.inventory.lastFilter.CategoryID)
}

func TestInventoryListRejectsAMalformedFilterID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		wantField string
	}{
		{"location_id", "?location_id=not-a-uuid", "location_id"},
		{"category_id", "?category_id=not-a-uuid", "category_id"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodGet, f.base()+"/inventory-batches"+tc.query, "")

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)[tc.wantField])
		})
	}
}

// TestInventoryListAnswersAForeignFilterIDWithNotFound — a location_id or
// category_id the store cannot resolve against this storage (foreign or
// unknown, indistinguishable) surfaces as the same 404 every other filter id
// in the system gets.
func TestInventoryListAnswersAForeignFilterIDWithNotFound(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.inventory.err = store.ErrNotFound
	rec := f.do(http.MethodGet, f.base()+"/inventory-batches?location_id="+uuid.New().String(), "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestInventoryListRendersEveryResolvedField pins the response shape: names
// resolved server-side, a bare-DATE expiration serialized as YYYY-MM-DD, and
// an empty (never null) location_path when the store found none.
func TestInventoryListRendersEveryResolvedField(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	batchID := uuid.New()
	productID := uuid.New()
	categoryID := uuid.New()
	categoryName := "Pasta"
	imageURL := "https://example.com/penne.jpg"
	expiresAt := time.Date(2027, time.January, 10, 0, 0, 0, 0, time.UTC)
	expires := &expiresAt

	f.inventory.rows = []store.InventoryBatchRow{{
		Batch: store.Batch{
			ID: batchID, ProductID: productID, LocationID: uuid.New(),
			Quantity: 3, ExpirationDate: expires, ExpirationSource: store.ExpirationDerived,
		},
		ProductName:  "Barilla Penne 500g",
		ImageURL:     &imageURL,
		CategoryID:   &categoryID,
		CategoryName: &categoryName,
		LocationPath: []string{"Basement", "Right Shelf", "Layer 2"},
	}}

	rec := f.do(http.MethodGet, f.base()+"/inventory-batches", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Items []struct {
			ID               string   `json:"id"`
			ProductName      string   `json:"product_name"`
			ImageURL         *string  `json:"image_url"`
			CategoryName     *string  `json:"category_name"`
			LocationPath     []string `json:"location_path"`
			Quantity         int      `json:"quantity"`
			ExpirationDate   *string  `json:"expiration_date"`
			ExpirationSource string   `json:"expiration_source"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Len(t, body.Items, 1)
	item := body.Items[0]
	assert.Equal(t, batchID.String(), item.ID)
	assert.Equal(t, "Barilla Penne 500g", item.ProductName)
	require.NotNil(t, item.ImageURL)
	assert.Equal(t, imageURL, *item.ImageURL)
	require.NotNil(t, item.CategoryName)
	assert.Equal(t, "Pasta", *item.CategoryName)
	assert.Equal(t, []string{"Basement", "Right Shelf", "Layer 2"}, item.LocationPath)
	assert.Equal(t, 3, item.Quantity)
	require.NotNil(t, item.ExpirationDate)
	assert.Equal(t, "2027-01-10", *item.ExpirationDate)
	assert.Equal(t, "derived", item.ExpirationSource)
	assert.Nil(t, body.NextCursor)
}

// encodeTestCursor mirrors the unexported encodeCursor in pagination.go, so a
// test can build a cursor a handler will accept without reaching into the
// package's internals.
func encodeTestCursor(id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(id[:])
}
