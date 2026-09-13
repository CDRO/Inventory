package httpapi_test

import (
	"context"
	"net/http"
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
	mu            sync.Mutex
	products      []store.Product
	productsErr   error
	batches       []store.Batch
	batchesErr    error
	lastStorageID uuid.UUID
	lastProductID uuid.UUID
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
