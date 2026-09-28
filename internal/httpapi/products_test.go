package httpapi_test

import (
	"bytes"
	"context"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

	delta      *store.Delta[store.Product]
	deltaErr   error
	lastSince  time.Time
	deltaCalls int

	// docs/specs/16-product-maintenance.md: the detail read, the full patch,
	// the merge and the delete.
	getErr            error
	currentStock      int
	currentStockErr   error
	logs              []store.ProductLog
	logsErr           error
	lastLogLimit      int
	lastPatch         *store.ProductPatch
	updateErr         error
	updateCalls       int
	recomputed        int
	movedBatches      int
	orphanedImage     string
	mergeErr          error
	mergeCalls        int
	lastMergeSourceID *uuid.UUID
	deleteErr         error
	deleteCalls       int

	// SearchProducts backs products.html's list table
	// (docs/specs/16-product-maintenance.md). searchResult, when set,
	// overrides the default (a name-substring filter over f.products).
	searchErr       error
	searchResult    []store.ProductSummary
	lastSearchQuery string
	searchCalls     int
}

func (f *fakeProductStore) ListProducts(_ context.Context, storageID uuid.UUID) ([]store.Product, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID = storageID
	return f.products, f.productsErr
}

func (f *fakeProductStore) SearchProducts(_ context.Context, storageID uuid.UUID, query string) ([]store.ProductSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastSearchQuery = storageID, query
	f.searchCalls++
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.searchResult != nil {
		return f.searchResult, nil
	}
	out := make([]store.ProductSummary, 0, len(f.products))
	for _, p := range f.products {
		if query != "" && !strings.Contains(strings.ToLower(p.Name), strings.ToLower(query)) {
			continue
		}
		out = append(out, store.ProductSummary{ID: p.ID, Name: p.Name})
	}
	return out, nil
}

func (f *fakeProductStore) ProductsChangedSince(_ context.Context, storageID uuid.UUID, since time.Time) (*store.Delta[store.Product], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID = storageID
	f.lastSince = since
	f.deltaCalls++
	if f.deltaErr != nil {
		return nil, f.deltaErr
	}
	if f.delta != nil {
		return f.delta, nil
	}
	return &store.Delta[store.Product]{Changed: f.products, Deleted: []uuid.UUID{}, SyncedAt: fixedSyncPoint}, nil
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

// The maintenance half of the fake (docs/specs/16-product-maintenance.md).
//
// getErr and friends default to store.ErrNotFound-free behaviour: a test that
// says nothing about them gets a product back, which keeps the older tests in
// this file untouched.

func (f *fakeProductStore) GetProduct(_ context.Context, storageID, id uuid.UUID) (*store.Product, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID = storageID, id
	if f.getErr != nil {
		return nil, f.getErr
	}
	for i := range f.products {
		if f.products[i].ID == id {
			p := f.products[i]
			return &p, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeProductStore) CurrentStock(_ context.Context, storageID, productID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID = storageID, productID
	return f.currentStock, f.currentStockErr
}

func (f *fakeProductStore) ListProductLogs(_ context.Context, storageID, productID uuid.UUID, limit int) ([]store.ProductLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID, f.lastLogLimit = storageID, productID, limit
	return f.logs, f.logsErr
}

func (f *fakeProductStore) UpdateProduct(_ context.Context, storageID, id uuid.UUID, patch store.ProductPatch) (*store.Product, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID, f.lastPatch = storageID, id, &patch
	f.updateCalls++
	if f.updateErr != nil {
		return nil, 0, f.updateErr
	}
	for i := range f.products {
		if f.products[i].ID == id {
			return &f.products[i], f.recomputed, nil
		}
	}
	return nil, 0, store.ErrNotFound
}

func (f *fakeProductStore) MergeProducts(_ context.Context, storageID, survivorID, sourceID uuid.UUID) (*store.MergeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID, f.lastMergeSourceID = storageID, survivorID, &sourceID
	f.mergeCalls++
	if f.mergeErr != nil {
		return nil, f.mergeErr
	}
	result := &store.MergeResult{MovedBatches: f.movedBatches, RecomputedBatches: f.recomputed, OrphanedImage: f.orphanedImage}
	for i := range f.products {
		if f.products[i].ID == survivorID {
			result.Survivor = &f.products[i]
		}
	}
	return result, nil
}

func (f *fakeProductStore) DeleteProduct(_ context.Context, storageID, id uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStorageID, f.lastProductID = storageID, id
	f.deleteCalls++
	return f.orphanedImage, f.deleteErr
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

// TestSearchProductsRoutesThroughQParam — products.html's list table
// (docs/specs/16-product-maintenance.md) reaches the same GET route as the
// plain list, distinguished only by ?q= being present at all: absent is
// today's id-and-name shape, present (even empty, "Show all products") is
// the richer summary shape.
func TestSearchProductsRoutesThroughQParam(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.products = []store.Product{{ID: uuid.New(), StorageID: f.storageID, Name: "Milk"}}

	rec := f.do(http.MethodGet, f.base()+"/products?q=milk", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.storageID, f.products.lastStorageID)
	assert.Equal(t, 1, f.products.searchCalls)
	assert.Contains(t, rec.Body.String(), `"current_stock"`, "the summary shape, not the plain id-and-name one")

	rec = f.do(http.MethodGet, f.base()+"/products?q=", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 2, f.products.searchCalls, "an empty q is still the summary route — \"show all\"")
	assert.Equal(t, "", f.products.lastSearchQuery)

	rec = f.do(http.MethodGet, f.base()+"/products", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 2, f.products.searchCalls, "no q at all is the plain id-and-name list, unchanged")
	assert.NotContains(t, rec.Body.String(), "current_stock")
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

// --- #248: the custom-upload change path ------------------------------------
//
// POST on the same address SetImage patches, which is the address
// docs/specs/07-shopping-list-reconciliation.md names for it. Every assertion
// below is about what reaches product storage and the store, not about the
// picker's half of the path.

// TestUploadImageStoresAnUprightStrippedPhoto is the invariant CLAUDE.md lists
// as failing silently, asserted where it actually matters: images.Strip being
// correct is worth nothing if this handler does not run the photo through it.
//
// The fixture is 8 wide, 4 tall and tagged "rotate 90", so the bytes that land
// in product storage must decode as 4x8 — the rotation baked into the pixels —
// and must carry no EXIF afterwards. A handler that stripped first and rotated
// never, or that wrote the original bytes, fails on one of the two.
func TestUploadImageStoresAnUprightStrippedPhoto(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	productID := uuid.New()
	contentType, body := multipartImage(t, "image", jpegWithEXIF(t, 8, 4, 6))

	rec := f.upload(http.MethodPost, f.base()+"/products/"+productID.String()+"/image", contentType, body)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.storageID, f.products.lastStorageID)
	assert.Equal(t, productID, f.products.lastProductID)
	assert.Equal(t, f.user.ID, f.products.lastActingUserID)
	// A photo replaces an icon rather than sitting beside one, exactly as the
	// picker's own PATCH body does.
	assert.Nil(t, f.products.lastIconName)

	require.NotNil(t, f.products.lastImageURL)
	prefix := f.base() + "/product-images/"
	require.True(t, strings.HasPrefix(*f.products.lastImageURL, prefix), *f.products.lastImageURL)

	stored := f.pictures.files[strings.TrimPrefix(*f.products.lastImageURL, prefix)]
	require.NotEmpty(t, stored, "the product must record a file that was actually written")
	assert.NotContains(t, string(stored), "Exif\x00\x00",
		"no photo may reach permanent storage with its metadata intact")

	cfg, err := jpeg.DecodeConfig(bytes.NewReader(stored))
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.Width, "the orientation must be applied to the pixels before stripping")
	assert.Equal(t, 8, cfg.Height)
}

// TestUploadImageNeverTakesTheNameOrURLTheCallerSupplied — the filename is
// generated from the bytes' own format, and no form field can talk this route
// into recording an address of the caller's choosing. The same guarantee
// TestSetImageNeverRecordsACallerSuppliedURL makes for the picker's PATCH.
func TestUploadImageNeverTakesTheNameOrURLTheCallerSupplied(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("image", "../../../etc/passwd.jpg")
	require.NoError(t, err)
	_, err = part.Write(jpegWithEXIF(t, 4, 4, 1))
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("image_url", "https://tracker.example/pixel.png"))
	require.NoError(t, writer.WriteField("icon_name", "box"))
	require.NoError(t, writer.Close())

	rec := f.upload(http.MethodPost, f.base()+"/products/"+uuid.NewString()+"/image",
		writer.FormDataContentType(), buf.Bytes())

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, f.products.lastImageURL)
	assert.True(t, strings.HasPrefix(*f.products.lastImageURL, f.base()+"/product-images/"),
		"got %q — the recorded address is generated, never supplied", *f.products.lastImageURL)
	assert.NotContains(t, *f.products.lastImageURL, "tracker.example")
	assert.NotContains(t, *f.products.lastImageURL, "passwd")
	assert.NotContains(t, *f.products.lastImageURL, "..")
	// A form field named like the PATCH's JSON body must not set an icon
	// either: this route takes one thing, the file.
	assert.Nil(t, f.products.lastIconName)

	for name := range f.pictures.files {
		assert.NotContains(t, name, "passwd", "the file on disk is named by the server")
		assert.NotContains(t, name, "/")
	}
}

// TestUploadImageGoesStraightToPermanentStorage — spec 07 says a custom photo
// "goes straight to permanent storage", and that unlike a provider image it is
// never written to a catalog entry. The suggestion cache is the tier a provider
// image arrives through and the only place a provider source URL — the value a
// catalog entry could record — comes from, so a custom upload that never
// touches it cannot reach one.
func TestUploadImageGoesStraightToPermanentStorage(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	contentType, body := multipartImage(t, "image", jpegWithEXIF(t, 4, 4, 1))

	rec := f.upload(http.MethodPost, f.base()+"/products/"+uuid.NewString()+"/image", contentType, body)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, f.pictures.files, 1, "the photo lands in permanent product storage")
	assert.Empty(t, f.imageData.fetched, "and never in the suggestion cache")
	assert.Empty(t, f.imageData.touched)
	assert.Empty(t, f.photos.files, "nor in the ingest area, which backs review jobs")
}

// TestUploadImageReportsNotFoundFromTheStoreAndKeepsNoFile — a product in
// another storage and one that does not exist are the same store.ErrNotFound
// and therefore the same 404 (docs/specs/03-auth-and-multi-tenancy.md). The
// file written before that row was refused must not survive it.
func TestUploadImageReportsNotFoundFromTheStoreAndKeepsNoFile(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.setImageErr = store.ErrNotFound
	contentType, body := multipartImage(t, "image", jpegWithEXIF(t, 4, 4, 1))

	rec := f.upload(http.MethodPost, f.base()+"/products/"+uuid.NewString()+"/image", contentType, body)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, "not_found", errorCode(t, rec))
	assert.Empty(t, f.pictures.files, "a refused write must leave no orphaned photo behind")
}

// TestUploadImageAnswers404ForAMalformedProductID — refused before the body is
// read at all, and with the same 404 every other malformed id in this package
// gets rather than a 422 that would say the id was at least examined.
func TestUploadImageAnswers404ForAMalformedProductID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	contentType, body := multipartImage(t, "image", jpegWithEXIF(t, 4, 4, 1))

	rec := f.upload(http.MethodPost, f.base()+"/products/not-a-uuid/image", contentType, body)

	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Empty(t, f.pictures.files)
}

// TestUploadImageRefusesWhatIsNotAJPEGOrPNG — the format comes from the bytes,
// so a text file named .jpg is refused on field `image` and nothing is written.
func TestUploadImageRefusesWhatIsNotAJPEGOrPNG(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	contentType, body := multipartImage(t, "image", []byte("this is not an image at all"))

	rec := f.upload(http.MethodPost, f.base()+"/products/"+uuid.NewString()+"/image", contentType, body)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.NotEmpty(t, errorFields(t, rec)["image"])
	assert.Empty(t, f.pictures.files)
	assert.Nil(t, f.products.lastImageURL, "and the store is never reached")
}

// TestUploadImageRefusesAMissingFile — a multipart form with no `image` part is
// the caller's mistake, reported on the field that is missing.
func TestUploadImageRefusesAMissingFile(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	contentType, body := multipartImage(t, "not-the-image", jpegWithEXIF(t, 4, 4, 1))

	rec := f.upload(http.MethodPost, f.base()+"/products/"+uuid.NewString()+"/image", contentType, body)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.NotEmpty(t, errorFields(t, rec)["image"])
	assert.Empty(t, f.pictures.files)
}

// TestUploadImageIsRefusedWithoutASession — the route is inside the
// session-and-membership group, so it is refused before any photo is read.
func TestUploadImageIsRefusedWithoutASession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	contentType, body := multipartImage(t, "image", jpegWithEXIF(t, 4, 4, 1))

	req := httptest.NewRequest(http.MethodPost, f.base()+"/products/"+uuid.NewString()+"/image",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Empty(t, f.pictures.files)
}
