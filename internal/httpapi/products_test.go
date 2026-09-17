package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// fakeProductStore is an in-memory ProductStore.
type fakeProductStore struct {
	mu               sync.Mutex
	products         []store.Product
	productsErr      error
	batches          []store.Batch
	batchesErr       error
	lastStorageID    uuid.UUID
	lastProductID    uuid.UUID
	setCategoryErr   error
	setImageErr      error
	lastCategoryID   *uuid.UUID
	lastImageURL     *string
	lastIconName     *string
	lastActingUserID uuid.UUID
}

func (f *fakeProductStore) ListProducts(_ context.Context, storageID uuid.UUID) ([]store.Product, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID = storageID
	return f.products, f.productsErr
}

func (f *fakeProductStore) ListProductBatches(_ context.Context, storageID, productID uuid.UUID) ([]store.Batch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID = storageID, productID
	return f.batches, f.batchesErr
}

func (f *fakeProductStore) SetProductCategoryAsUser(_ context.Context, storageID, id uuid.UUID, categoryID *uuid.UUID, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID, f.lastCategoryID, f.lastActingUserID = storageID, id, categoryID, userID
	return f.setCategoryErr
}

func (f *fakeProductStore) SetProductImageAsUser(_ context.Context, storageID, id uuid.UUID, imageURL, iconName *string, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID, f.lastImageURL, f.lastIconName, f.lastActingUserID = storageID, id, imageURL, iconName, userID
	return f.setImageErr
}

// TestListProductsExposesOnlyIDAndName — category_id and catalog_id are
// server-side bookkeeping (docs/specs/02-data-model.md) and must never reach
// a response, even from a route whose only job is naming products for a
// picker.
func TestListProductsExposesOnlyIDAndName(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	categoryID, catalogID := uuid.New(), uuid.New()
	f.products.products = []store.Product{
		{ID: uuid.New(), StorageID: f.storageID, Name: "Beans", CategoryID: &categoryID, CatalogID: &catalogID},
		{ID: uuid.New(), StorageID: f.storageID, Name: "Apples"},
	}

	rec := f.do(http.MethodGet, f.base()+"/products", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.storageID, f.products.lastStorageID)

	body := rec.Body.String()
	assert.Contains(t, body, `"name":"Beans"`)
	assert.Contains(t, body, `"name":"Apples"`)
	assert.NotContains(t, body, categoryID.String())
	assert.NotContains(t, body, catalogID.String())
	assert.NotContains(t, body, "category_id")
	assert.NotContains(t, body, "catalog_id")
}

// TestListProductBatchesIsStorageScoped — a product in another storage is
// ErrNotFound from the store, and the route answers the same 404 as any other
// foreign or nonexistent id.
func TestListProductBatchesIsStorageScopedAndOrdered(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	productID := uuid.New()
	near := time.Now().AddDate(0, 0, 3)
	far := time.Now().AddDate(0, 0, 30)
	f.products.batches = []store.Batch{
		{ID: uuid.New(), ProductID: productID, Quantity: 2, ExpirationDate: &near},
		{ID: uuid.New(), ProductID: productID, Quantity: 5, ExpirationDate: &far},
	}

	rec := f.do(http.MethodGet, f.base()+"/products/"+productID.String()+"/batches", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.storageID, f.products.lastStorageID)
	assert.Equal(t, productID, f.products.lastProductID)
	assert.Contains(t, rec.Body.String(), `"quantity":2`)

	f.products.batchesErr = store.ErrNotFound
	notFound := f.do(http.MethodGet, f.base()+"/products/"+uuid.NewString()+"/batches", "")
	assert.Equal(t, http.StatusNotFound, notFound.Code)
}

// TestSetCategorySendsTheParsedIDAndActingUser — the "uncategorized" quest
// (docs/specs/52-gamification-quests-and-ui.md) is only closeable if this
// route actually reaches the store with the caller's own id, not a blank one.
func TestSetCategorySendsTheParsedIDAndActingUser(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	productID := uuid.New()
	categoryID := uuid.New()

	rec := f.do(http.MethodPatch, f.base()+"/products/"+productID.String()+"/category",
		`{"category_id":"`+categoryID.String()+`"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, productID, f.products.lastProductID)
	require.NotNil(t, f.products.lastCategoryID)
	assert.Equal(t, categoryID, *f.products.lastCategoryID)
	assert.Equal(t, f.user.ID, f.products.lastActingUserID)
}

// TestSetCategoryAcceptsNullToClear — category_id is nullable by design
// (a product can be uncategorized again), so null must not be treated as a
// malformed request.
func TestSetCategoryAcceptsNullToClear(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/products/"+uuid.NewString()+"/category", `{"category_id":null}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Nil(t, f.products.lastCategoryID)
}

func TestSetCategoryRejectsAMalformedID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/products/"+uuid.NewString()+"/category", `{"category_id":"not-a-uuid"}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestSetCategoryReportsNotFoundFromTheStore(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.setCategoryErr = store.ErrNotFound

	rec := f.do(http.MethodPatch, f.base()+"/products/"+uuid.NewString()+"/category", `{"category_id":null}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestSetImagePromotesThePickedSuggestion — the "imageless" quest
// (docs/specs/52-gamification-quests-and-ui.md) needs this route to give a
// product a picture, and spec 07 needs that picture to be a permanent copy:
// a suggestion-cache address recorded on a product can be evicted from under
// it.
func TestSetImagePromotesThePickedSuggestion(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	productID := uuid.New()
	hash := strings.Repeat("b", 64)
	f.imageData.sources = map[string]string{hash: "https://provider.test/soup.png"}
	f.imageData.data, f.imageData.contentType = []byte("\x89PNGsoup"), "image/png"

	rec := f.do(http.MethodPatch, f.base()+"/products/"+productID.String()+"/image",
		`{"image":"`+hash+`","icon_name":null}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, productID, f.products.lastProductID)
	assert.Nil(t, f.products.lastIconName)
	assert.Equal(t, f.user.ID, f.products.lastActingUserID)

	require.NotNil(t, f.products.lastImageURL)
	prefix := f.base() + "/product-images/"
	require.True(t, strings.HasPrefix(*f.products.lastImageURL, prefix), *f.products.lastImageURL)
	assert.Equal(t, []byte("\x89PNGsoup"), f.pictures.files[strings.TrimPrefix(*f.products.lastImageURL, prefix)],
		"the product records a permanent copy of the suggestion's bytes")
}

// TestSetImageNeverRecordsACallerSuppliedURL — neither a suggestion-cache
// address nor anyone else's server can become a product's picture through
// this route.
func TestSetImageNeverRecordsACallerSuppliedURL(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	for _, url := range []string{"/api/storages/x/images/abc", "https://tracker.example/pixel.png"} {
		rec := f.do(http.MethodPatch, f.base()+"/products/"+uuid.NewString()+"/image",
			`{"image_url":"`+url+`","icon_name":"box"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Nil(t, f.products.lastImageURL, url)
	}
	assert.Empty(t, f.pictures.files)
}

// TestSetImageRefusesASuggestionNoLongerCached — the person picked a picture
// that was evicted since, and is asked to pick again rather than getting a
// product with a broken picture.
func TestSetImageRefusesASuggestionNoLongerCached(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodPatch, f.base()+"/products/"+uuid.NewString()+"/image",
		`{"image":"`+strings.Repeat("c", 64)+`"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.NotEmpty(t, errorFields(t, rec)["image"])
	assert.Empty(t, f.pictures.files)
}

func TestSetImageReportsNotFoundFromTheStore(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.setImageErr = store.ErrNotFound

	rec := f.do(http.MethodPatch, f.base()+"/products/"+uuid.NewString()+"/image", `{"icon_name":"box"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
