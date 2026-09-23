package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/config"
	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// The storage id a handler acts on is only ever the one RequireStorageMember
// put in the request context, and that context key is unexported. There is no
// way to hand a handler an unvalidated id from a test, which is the same
// guarantee production code has — so every test here drives the real router
// through the real gate chain rather than calling a handler directly.

// fakeLocations is an in-memory LocationStore that records what it was asked
// to do, so a test can assert on the arguments a handler derived from a request
// rather than only on the response.
type fakeLocations struct {
	tree    []store.Location
	treeErr error

	created    *store.Location
	createErr  error
	lastCreate store.NewLocation

	updated   *store.Location
	updateErr error
	lastPatch store.LocationPatch
	lastID    uuid.UUID

	deleteErr error
	deleted   []uuid.UUID

	lastStorageID uuid.UUID

	// creates counts CreateLocation calls, which is how the idempotency tests
	// tell a replay from a second execution.
	creates atomic.Int32

	delta      *store.Delta[store.Location]
	deltaErr   error
	lastSince  time.Time
	deltaCalls int
}

func (f *fakeLocations) LocationTree(_ context.Context, storageID uuid.UUID) ([]store.Location, error) {
	f.lastStorageID = storageID
	return f.tree, f.treeErr
}

func (f *fakeLocations) LocationsChangedSince(_ context.Context, storageID uuid.UUID, since time.Time) (*store.Delta[store.Location], error) {
	f.lastStorageID = storageID
	f.lastSince = since
	f.deltaCalls++
	if f.deltaErr != nil {
		return nil, f.deltaErr
	}
	if f.delta != nil {
		return f.delta, nil
	}
	return &store.Delta[store.Location]{Changed: f.tree, Deleted: []uuid.UUID{}, SyncedAt: fixedSyncPoint}, nil
}

func (f *fakeLocations) CreateLocationAsUser(_ context.Context, storageID uuid.UUID, in store.NewLocation, _ uuid.UUID) (*store.Location, error) {
	f.creates.Add(1)
	f.lastStorageID = storageID
	f.lastCreate = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &store.Location{ID: uuid.New(), StorageID: storageID, Name: in.Name, Description: in.Description, ParentID: in.ParentID}, nil
}

func (f *fakeLocations) UpdateLocation(_ context.Context, storageID, id uuid.UUID, patch store.LocationPatch) (*store.Location, error) {
	f.lastStorageID = storageID
	f.lastID = id
	f.lastPatch = patch
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.updated != nil {
		return f.updated, nil
	}
	return &store.Location{ID: id, StorageID: storageID, Name: "Updated"}, nil
}

func (f *fakeLocations) DeleteLocation(_ context.Context, storageID, id uuid.UUID) error {
	f.lastStorageID = storageID
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

// fakeBatches is an in-memory BatchStore, recording the same way.
type fakeBatches struct {
	split    *store.Batch
	splitErr error
	moved    *store.Batch
	moveErr  error

	lastStorageID  uuid.UUID
	lastBatchID    uuid.UUID
	lastQuantity   int
	lastTargetID   uuid.UUID
	lastPatch      store.BatchPatch
	lastActingUser *uuid.UUID
}

func (f *fakeBatches) SplitBatch(_ context.Context, storageID, batchID uuid.UUID, quantity int, target uuid.UUID, userID *uuid.UUID) (*store.Batch, error) {
	f.lastStorageID, f.lastBatchID, f.lastQuantity, f.lastTargetID, f.lastActingUser = storageID, batchID, quantity, target, userID
	if f.splitErr != nil {
		return nil, f.splitErr
	}
	if f.split != nil {
		return f.split, nil
	}
	return &store.Batch{ID: uuid.New(), LocationID: target, Quantity: quantity}, nil
}

func (f *fakeBatches) UpdateBatch(_ context.Context, storageID, batchID uuid.UUID, patch store.BatchPatch, userID *uuid.UUID) (*store.Batch, error) {
	f.lastStorageID, f.lastBatchID, f.lastActingUser = storageID, batchID, userID
	f.lastPatch = patch
	if patch.LocationID != nil {
		f.lastTargetID = *patch.LocationID
	}
	if f.moveErr != nil {
		return nil, f.moveErr
	}
	if patch.Quantity != nil && *patch.Quantity == 0 {
		// The store deletes an emptied batch, so there is no row to return —
		// the handler turns that into a 204.
		return nil, nil
	}
	if f.moved != nil {
		return f.moved, nil
	}
	moved := &store.Batch{ID: batchID, LocationID: f.lastTargetID, Quantity: 3}
	if patch.Quantity != nil {
		moved.Quantity = *patch.Quantity
	}
	return moved, nil
}

// fakeStocktake is an in-memory StocktakeStore
// (docs/specs/13-stocktake-and-audit.md), recording the same way.
type fakeStocktake struct {
	created    *store.Batch
	createErr  error
	lastCreate store.NewBatch

	sheet    *store.StocktakeSheet
	sheetErr error

	confirmed     *store.StocktakeResult
	confirmErr    error
	lastCounts    []store.StocktakeCount
	lastFound     []store.FoundStock
	lastLocation  uuid.UUID
	lastStorageID uuid.UUID
	lastUser      *uuid.UUID
}

func (f *fakeStocktake) CreateBatch(_ context.Context, storageID uuid.UUID, in store.NewBatch) (*store.Batch, error) {
	f.lastStorageID = storageID
	f.lastCreate = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &store.Batch{
		ID: uuid.New(), ProductID: in.ProductID, LocationID: in.LocationID,
		Quantity: in.Quantity, ExpirationDate: in.ExpirationDate,
		ExpirationSource: in.ExpirationSource,
	}, nil
}

func (f *fakeStocktake) LocationStocktake(_ context.Context, storageID, locationID uuid.UUID) (*store.StocktakeSheet, error) {
	f.lastStorageID, f.lastLocation = storageID, locationID
	if f.sheetErr != nil {
		return nil, f.sheetErr
	}
	if f.sheet != nil {
		return f.sheet, nil
	}
	return &store.StocktakeSheet{
		Location: store.Location{ID: locationID, StorageID: storageID, Name: "Layer 2"},
		Batches:  []store.StocktakeBatch{},
	}, nil
}

func (f *fakeStocktake) ConfirmStocktake(_ context.Context, storageID, locationID uuid.UUID, userID *uuid.UUID, counts []store.StocktakeCount, found []store.FoundStock) (*store.StocktakeResult, error) {
	f.lastStorageID, f.lastLocation, f.lastUser = storageID, locationID, userID
	f.lastCounts, f.lastFound = counts, found
	if f.confirmErr != nil {
		return nil, f.confirmErr
	}
	if f.confirmed != nil {
		return f.confirmed, nil
	}
	return &store.StocktakeResult{CreatedBatchIDs: []uuid.UUID{}}, nil
}

// fakeExportStore is an in-memory ExportStore
// (docs/specs/15-backup-restore-and-export.md).
type fakeExportStore struct {
	data      *store.StorageExport
	err       error
	lastID    uuid.UUID
	callCount int
}

func (f *fakeExportStore) ExportStorage(_ context.Context, storageID uuid.UUID) (*store.StorageExport, error) {
	f.callCount++
	f.lastID = storageID
	if f.err != nil {
		return nil, f.err
	}
	if f.data != nil {
		return f.data, nil
	}
	return &store.StorageExport{Storage: store.Storage{ID: storageID, Name: "Kitchen"}}, nil
}

// fakeAPI is the whole APIStore: the authorization lookups and the two
// resources behind them.
type fakeAPI struct {
	*fakeAuth
	*fakeLocations
	*fakeCategories
	*fakeBatches
	*fakeShoppingLists
	*fakeExpiry
	*fakeJobs
	*fakeIdempotency
	*fakeIngestStore
	*fakeConsumeStore
	*fakeProductStore
	*fakeReorderStore
	*fakeAnalyticsStore
	*fakeGamification
	*fakeStocktake
	*fakeNotifications
	*fakeExportStore
	*fakeBarcodes
	*fakeBarcodePrompt
}

// newFakeAPI builds the whole fake store around an auth fake, with every other
// resource empty.
func newFakeAPI(auth *fakeAuth) fakeAPI {
	return fakeAPI{
		fakeAuth: auth, fakeLocations: &fakeLocations{}, fakeCategories: &fakeCategories{}, fakeBatches: &fakeBatches{},
		fakeShoppingLists: &fakeShoppingLists{}, fakeExpiry: &fakeExpiry{},
		fakeJobs: newFakeJobs(), fakeIdempotency: newFakeIdempotency(), fakeIngestStore: &fakeIngestStore{},
		fakeConsumeStore: &fakeConsumeStore{}, fakeProductStore: &fakeProductStore{},
		fakeReorderStore: &fakeReorderStore{}, fakeAnalyticsStore: &fakeAnalyticsStore{},
		fakeGamification: newFakeGamification(), fakeStocktake: &fakeStocktake{},
		fakeNotifications: &fakeNotifications{},
		fakeExportStore:   &fakeExportStore{},
		fakeBarcodes:      newFakeBarcodes(),
		fakeBarcodePrompt: newFakeBarcodePrompt(),
	}
}

// apiFixture builds a router with a member session already established, and
// returns everything a test needs to make a request as that member.
type apiFixture struct {
	router        http.Handler
	auth          *fakeAuth
	locations     *fakeLocations
	categories    *fakeCategories
	batches       *fakeBatches
	lists         *fakeShoppingLists
	expiry        *fakeExpiry
	jobs          *fakeJobs
	idem          *fakeIdempotency
	ingest        *fakeIngestStore
	ingester      *fakeIngester
	photos        *fakePhotoStore
	pictures      *fakePhotoStore
	matcher       *fakeMatcher
	images        *fakeSuggester
	imageData     *fakeImageCache
	consume       *fakeConsumeStore
	consumer      *fakeConsumer
	products      *fakeProductStore
	reorder       *fakeReorderStore
	analytics     *fakeAnalyticsStore
	gamification  *fakeGamification
	stocktake     *fakeStocktake
	notifications *fakeNotifications
	notifier      *fakeNotifier
	exports       *fakeExportStore
	adminVision   *fakeAdminVision
	barcodes      *fakeBarcodes
	barcodePrompt *fakeBarcodePrompt
	storageID     uuid.UUID
	user          *store.User
	session       *store.Session
}

// newAPIFixture builds the fixture. Each opt may adjust the router's
// dependencies before the router is built — to drop one, say, and test how
// the API degrades without it.
func newAPIFixture(t *testing.T, opts ...func(*httpapi.Deps)) *apiFixture {
	t.Helper()

	auth := newFakeAuth()
	locations := &fakeLocations{}
	categories := &fakeCategories{}
	batches := &fakeBatches{}
	lists := &fakeShoppingLists{}
	expiry := &fakeExpiry{}
	matcher := &fakeMatcher{}
	images := &fakeSuggester{}
	imageData := &fakeImageCache{}
	gamification := newFakeGamification()
	stocktake := &fakeStocktake{}
	notifications := &fakeNotifications{}
	notifier := &fakeNotifier{}
	exports := &fakeExportStore{}
	barcodes := newFakeBarcodes()
	barcodePrompt := newFakeBarcodePrompt()
	user, session := auth.addUser(t, false)
	storageID := uuid.New()
	auth.addMember(storageID, user.ID)

	jobs := newFakeJobs()
	idem := newFakeIdempotency()
	ingestStore := &fakeIngestStore{}
	ingester := &fakeIngester{available: true}
	photos := &fakePhotoStore{files: map[string][]byte{}}
	pictures := &fakePhotoStore{files: map[string][]byte{}}
	consumeStore := &fakeConsumeStore{}
	consumer := &fakeConsumer{available: true}
	products := &fakeProductStore{}
	reorder := &fakeReorderStore{}
	analytics := &fakeAnalyticsStore{}
	adminVision := &fakeAdminVision{status: "ok"}

	deps := httpapi.Deps{
		DB:     stubPinger{},
		Vision: stubVision{status: "ok"},
		Errors: httpapi.NewErrorWriter(false, discardLogger()),
		Store: fakeAPI{
			fakeAuth: auth, fakeLocations: locations, fakeCategories: categories,
			fakeBatches: batches, fakeShoppingLists: lists, fakeExpiry: expiry,
			fakeJobs: jobs, fakeIdempotency: idem, fakeIngestStore: ingestStore,
			fakeConsumeStore: consumeStore, fakeProductStore: products,
			fakeReorderStore: reorder, fakeAnalyticsStore: analytics,
			fakeGamification: gamification, fakeStocktake: stocktake,
			fakeNotifications: notifications,
			fakeExportStore:   exports,
			fakeBarcodes:      barcodes,
			fakeBarcodePrompt: barcodePrompt,
		},
		Matcher:       matcher,
		Images:        images,
		ImageCache:    imageData,
		Ingester:      ingester,
		Photos:        photos,
		ProductImages: pictures,
		Consumer:      consumer,
		AdminVision:   adminVision,
		Notifier:      notifier,
		Config:        &config.Config{GeminiModel: "gemini-2.0-flash", AppEnv: "dev", HTTPPort: "8000"},
	}
	for _, opt := range opts {
		opt(&deps)
	}
	router := httpapi.NewRouter(deps)

	return &apiFixture{
		router: router, auth: auth, locations: locations, categories: categories, batches: batches,
		lists: lists, expiry: expiry, jobs: jobs, idem: idem,
		ingest: ingestStore, ingester: ingester, photos: photos, pictures: pictures,
		matcher: matcher, images: images, imageData: imageData,
		consume: consumeStore, consumer: consumer, products: products,
		reorder:       reorder,
		analytics:     analytics,
		gamification:  gamification,
		stocktake:     stocktake,
		notifications: notifications,
		notifier:      notifier,
		exports:       exports,
		adminVision:   adminVision,
		barcodes:      barcodes,
		barcodePrompt: barcodePrompt,
		storageID:     storageID, user: user, session: session,
	}
}

// do issues a request carrying the fixture's session cookie.
func (f *apiFixture) do(method, path, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: f.session.ID})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// anonymous issues the same request with no session at all.
func (f *apiFixture) anonymous(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *apiFixture) base() string {
	return "/api/storages/" + f.storageID.String()
}

// errorCode reads the code out of the one error envelope.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code   string              `json:"code"`
			Fields map[string][]string `json:"fields"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "every error is the one JSON envelope")
	return body.Error.Code
}

func errorFields(t *testing.T, rec *httptest.ResponseRecorder) map[string][]string {
	t.Helper()
	var body struct {
		Error struct {
			Fields map[string][]string `json:"fields"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Error.Fields
}

// storageRoutes is every storage-scoped route this PR registers. The tests
// below iterate it so that a route added later without the gate chain in front
// of it fails here rather than shipping open.
func storageRoutes(base string) []struct {
	method string
	path   string
	body   string
} {
	id := uuid.New().String()
	return []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, base + "/locations", ""},
		{http.MethodGet, base + "/categories", ""},
		{http.MethodPost, base + "/categories", `{"name":"Food"}`},
		{http.MethodPatch, base + "/categories/" + id, `{"name":"Food"}`},
		{http.MethodDelete, base + "/categories/" + id, ""},
		{http.MethodPost, base + "/locations", `{"name":"Cellar"}`},
		{http.MethodPatch, base + "/locations/" + id, `{"name":"Cellar"}`},
		{http.MethodDelete, base + "/locations/" + id, ""},
		{http.MethodPatch, base + "/inventory-batches/" + id, `{"location_id":"` + id + `"}`},
		{http.MethodPost, base + "/inventory-batches/" + id + "/split", `{"quantity":1,"target_location_id":"` + id + `"}`},
		{http.MethodGet, base + "/dashboard/reorder", ""},
		{http.MethodGet, base + "/dashboard/reorder/export", ""},
		{http.MethodPost, base + "/dashboard/reorder/items/match", `{"name":"Butter"}`},
		{http.MethodPost, base + "/dashboard/reorder/items", `{"name":"Butter"}`},
		{http.MethodGet, base + "/dashboard/analytics", ""},
		// Stocktake and manual inventory correction
		// (docs/specs/13-stocktake-and-audit.md).
		{http.MethodPost, base + "/inventory-batches", `{"product_id":"` + id + `","location_id":"` + id + `","quantity":1}`},
		{http.MethodGet, base + "/locations/" + id + "/stocktake", ""},
		{http.MethodPost, base + "/locations/" + id + "/stocktake", `{"batches":[]}`},
		// Member export (docs/specs/15-backup-restore-and-export.md).
		{http.MethodGet, base + "/export", ""},
		// Barcode recall (docs/specs/20-barcode-recall.md).
		{http.MethodGet, base + "/products/" + id + "/barcodes", ""},
		{http.MethodPost, base + "/products/" + id + "/barcodes", `{"barcode":"4006381333931"}`},
		{http.MethodDelete, base + "/products/" + id + "/barcodes/4006381333931", ""},
		{http.MethodGet, base + "/barcodes/4006381333931", ""},
		{http.MethodPost, base + "/barcodes/4006381333931/log", `{"direction":"out","quantity":1}`},
		{http.MethodPost, base + "/barcodes/4006381333931/product", ""},
		{http.MethodPost, base + "/barcodes/decode", ""},
	}
}

// TestStorageScopedRoutesRequireASession is the wiring test for the claim in
// the package doc: adding a route under /api/storages/{storage_id} is the same
// act as protecting it. A route registered outside the sub-router would answer
// something other than 401 here.
func TestStorageScopedRoutesRequireASession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	for _, route := range storageRoutes(f.base()) {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := f.anonymous(route.method, route.path, route.body)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "unauthorized", errorCode(t, rec))
		})
	}
}

// TestStorageScopedRoutesAre404ForANonMember pins the non-enumeration rule on
// every route at once: a caller with a valid session who is not a member of the
// storage gets the same 404 an unknown storage id gets, with nothing in the
// body to tell the two apart.
func TestStorageScopedRoutesAre404ForANonMember(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.removeMember(f.storageID, f.user.ID)

	unknown := "/api/storages/" + uuid.New().String()

	for _, route := range storageRoutes(f.base()) {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			mine := f.do(route.method, route.path, route.body)
			require.Equal(t, http.StatusNotFound, mine.Code)
			assert.Equal(t, "not_found", errorCode(t, mine))

			// The same request against a storage id that names nothing at all.
			other := strings.Replace(route.path, f.base(), unknown, 1)
			theirs := f.do(route.method, other, route.body)

			assert.Equal(t, mine.Code, theirs.Code)
			assert.JSONEq(t, mine.Body.String(), theirs.Body.String(),
				"an inaccessible storage and a nonexistent one must be byte-identical")
		})
	}
}

// TestProductionResponseCarriesNoDebugReason guards the one-serializer rule
// from the new handlers' side: these routes produce plenty of 404s, and none of
// them may name the check that failed when APP_ENV is not dev.
func TestProductionResponseCarriesNoDebugReason(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.removeMember(f.storageID, f.user.ID)

	rec := f.do(http.MethodGet, f.base()+"/locations", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "debug_reason")
	assert.NotContains(t, rec.Body.String(), "not_storage_member")
}

// TestLocationTreeNestsChildrenWhateverTheRowOrder is the regression for the
// subtlety in nestLocations: LocationTree orders by created_at, which stops
// putting parents first the moment an old node is re-parented under a newer
// one. A single-pass build would silently drop that subtree — the nodes would
// still exist, still hold inventory, and simply not render.
func TestLocationTreeNestsChildrenWhateverTheRowOrder(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	parent := uuid.New()
	child := uuid.New()

	// Child first, exactly as created_at would order it after the parent was
	// created later and the child moved underneath it.
	f.locations.tree = []store.Location{
		{ID: child, StorageID: f.storageID, ParentID: &parent, Name: "Layer 2"},
		{ID: parent, StorageID: f.storageID, Name: "Right Shelf"},
	}

	rec := f.do(http.MethodGet, f.base()+"/locations", "")
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

// TestLocationTreeResponseNeverCarriesStorageID asserts on the wire format,
// not on the Go type: the spec's requirement is that no node from another
// storage appear at any depth, and the strongest version of that is a response
// in which storage_id is not a field a client can read at all.
func TestLocationTreeResponseNeverCarriesStorageID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	parent := uuid.New()
	f.locations.tree = []store.Location{
		{ID: parent, StorageID: f.storageID, Name: "Basement"},
		{ID: uuid.New(), StorageID: f.storageID, ParentID: &parent, Name: "Shelf"},
	}

	rec := f.do(http.MethodGet, f.base()+"/locations", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "storage_id")
	assert.NotContains(t, rec.Body.String(), f.storageID.String())
}

// TestLocationTreeLeafHasAnEmptyChildrenArray keeps the client from having to
// treat a leaf as a special case: children is always an array, never null.
func TestLocationTreeLeafHasAnEmptyChildrenArray(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.locations.tree = []store.Location{{ID: uuid.New(), StorageID: f.storageID, Name: "Shed"}}

	rec := f.do(http.MethodGet, f.base()+"/locations", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"children":[]`)
}

func TestCreateLocationValidatesName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"empty", `{"name":""}`},
		{"whitespace only", `{"name":"   "}`},
		{"too long", `{"name":"` + strings.Repeat("x", 256) + `"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/locations", tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, "validation_failed", errorCode(t, rec))
			assert.NotEmpty(t, errorFields(t, rec)["name"], "the form needs to know which field to mark")
		})
	}
}

// TestCreateLocationTakesStorageFromTheURLNotTheBody is the structural version
// of "the new node inherits the URL's storage_id". A body that names another
// storage changes nothing, because there is no field on the store's input that
// could carry one.
func TestCreateLocationTakesStorageFromTheURLNotTheBody(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	foreign := uuid.New()

	rec := f.do(http.MethodPost, f.base()+"/locations",
		`{"name":"Cellar","storage_id":"`+foreign.String()+`"}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, f.storageID, f.locations.lastStorageID,
		"the store must be called with the storage the middleware validated")
	assert.NotEqual(t, foreign, f.locations.lastStorageID)
}

func TestCreateLocationRejectsAMalformedParentID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/locations", `{"name":"Cellar","parent_id":"not-a-uuid"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotEmpty(t, errorFields(t, rec)["parent_id"])
}

// TestUpdateLocationSeparatesAbsentFromNull is the reason the patch type has
// presence booleans at all. Omitting parent_id must leave the node where it is;
// sending null must move it to the root. Collapsing the two would make it
// impossible to promote a node to the top of the tree.
func TestUpdateLocationSeparatesAbsentFromNull(t *testing.T) {
	t.Parallel()

	newParent := uuid.New()

	tests := []struct {
		name            string
		body            string
		wantSetParent   bool
		wantParentIsNil bool
		wantSetDesc     bool
	}{
		{
			name:          "parent_id omitted leaves the parent alone",
			body:          `{"name":"Renamed"}`,
			wantSetParent: false,
		},
		{
			name:            "parent_id null makes it a root",
			body:            `{"parent_id":null}`,
			wantSetParent:   true,
			wantParentIsNil: true,
		},
		{
			name:          "parent_id set re-parents",
			body:          `{"parent_id":"` + newParent.String() + `"}`,
			wantSetParent: true,
		},
		{
			name:        "description null clears it",
			body:        `{"description":null}`,
			wantSetDesc: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPatch, f.base()+"/locations/"+uuid.New().String(), tc.body)
			require.Equal(t, http.StatusOK, rec.Code)

			patch := f.locations.lastPatch
			assert.Equal(t, tc.wantSetParent, patch.SetParentID)
			assert.Equal(t, tc.wantSetDesc, patch.SetDescription)
			if tc.wantSetParent && tc.wantParentIsNil {
				assert.Nil(t, patch.ParentID, "null means root, not unchanged")
			}
			if tc.wantSetParent && !tc.wantParentIsNil {
				require.NotNil(t, patch.ParentID)
				assert.Equal(t, newParent, *patch.ParentID)
			}
		})
	}
}

// TestUpdateLocationDoesNotClobberUnmentionedFields is the trap this patch
// shape exists to avoid: a rename that also silently erased the description
// would be a data loss no error reports.
func TestUpdateLocationDoesNotClobberUnmentionedFields(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/locations/"+uuid.New().String(), `{"name":"Renamed"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, f.locations.lastPatch.SetDescription,
		"a body that never mentioned description must not rewrite it")
	assert.False(t, f.locations.lastPatch.SetParentID)
	require.NotNil(t, f.locations.lastPatch.Name)
	assert.Equal(t, "Renamed", *f.locations.lastPatch.Name)
}

// TestMalformedResourceIDIs404NotBadRequest keeps the enumeration rule intact
// one level below the storage id: answering 400 for a badly-shaped id would
// tell a prober that a well-formed guess is worth making.
func TestMalformedResourceIDIs404NotBadRequest(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPatch, f.base() + "/locations/not-a-uuid", `{"name":"x"}`},
		{http.MethodDelete, f.base() + "/locations/not-a-uuid", ""},
		{http.MethodPatch, f.base() + "/inventory-batches/not-a-uuid", `{"location_id":"` + uuid.New().String() + `"}`},
		{http.MethodPost, f.base() + "/inventory-batches/not-a-uuid/split", `{"quantity":1,"target_location_id":"` + uuid.New().String() + `"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := f.do(tc.method, tc.path, tc.body)

			require.Equal(t, http.StatusNotFound, rec.Code)
			assert.Equal(t, "not_found", errorCode(t, rec))
		})
	}
}

// TestStoreErrorsMapToTheSpecStatusCodes covers the table in
// docs/specs/04-backend-api-conventions.md for these routes: a foreign or
// missing id is 404 and never 403, and a legal row in an illegal state is 409.
func TestStoreErrorsMapToTheSpecStatusCodes(t *testing.T) {
	t.Parallel()

	t.Run("a location in another storage is 404, not 403", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.locations.updateErr = store.ErrNotFound

		rec := f.do(http.MethodPatch, f.base()+"/locations/"+uuid.New().String(), `{"name":"Mine Now"}`)

		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.NotEqual(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, "not_found", errorCode(t, rec))
	})

	t.Run("deleting a location that still holds inventory is 409", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.locations.deleteErr = store.ErrConflict

		rec := f.do(http.MethodDelete, f.base()+"/locations/"+uuid.New().String(), "")

		require.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, "conflict", errorCode(t, rec))
	})

	t.Run("a cycle is 409", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.locations.updateErr = store.ErrConflict

		rec := f.do(http.MethodPatch, f.base()+"/locations/"+uuid.New().String(),
			`{"parent_id":"`+uuid.New().String()+`"}`)

		require.Equal(t, http.StatusConflict, rec.Code)
	})
}

// TestConflictMessagesAreWrittenForPeople guards against the store's own error
// text becoming UI copy. FromStoreError puts the wrapped store error in
// `message`, which reads as "store: conflict: location still holds 2 inventory
// batch(es)" — an internal string, prefix and all, in front of somebody trying
// to tidy a pantry. The internal text belongs in the log and in dev output,
// which is the serializer's decision, not the handler's.
func TestConflictMessagesAreWrittenForPeople(t *testing.T) {
	t.Parallel()

	message := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.Error.Message
	}

	t.Run("delete", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.locations.deleteErr = fmt.Errorf("%w: location still holds 2 inventory batch(es)", store.ErrConflict)

		rec := f.do(http.MethodDelete, f.base()+"/locations/"+uuid.New().String(), "")

		require.Equal(t, http.StatusConflict, rec.Code)
		assert.NotContains(t, message(t, rec), "store:")
		assert.Contains(t, message(t, rec), "still holds inventory")
	})

	t.Run("cycle", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.locations.updateErr = fmt.Errorf("%w: re-parenting would create a cycle in locations", store.ErrConflict)

		rec := f.do(http.MethodPatch, f.base()+"/locations/"+uuid.New().String(),
			`{"parent_id":"`+uuid.New().String()+`"}`)

		require.Equal(t, http.StatusConflict, rec.Code)
		assert.NotContains(t, message(t, rec), "store:")
		assert.NotContains(t, message(t, rec), "re-parenting")
	})
}

func TestDeleteLocationAnswers204WithNoBody(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	id := uuid.New()

	rec := f.do(http.MethodDelete, f.base()+"/locations/"+id.String(), "")

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, []uuid.UUID{id}, f.locations.deleted)
}

// TestOversizedJSONBodyIs413 covers decodeJSON's size cap, the one path in
// respond.go that nothing else exercises. Without the cap a caller can make the
// server buffer a body of their choosing, which is a denial of service costing
// the attacker a single request; without this test, the cap could be removed
// and every other test would still pass.
func TestOversizedJSONBodyIs413(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	// Comfortably past the 64 KiB cap, and valid JSON, so a rejection can only
	// come from the size limit rather than from the decoder giving up.
	body := `{"name":"` + strings.Repeat("x", 128<<10) + `"}`

	rec := f.do(http.MethodPost, f.base()+"/locations", body)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "payload_too_large", errorCode(t, rec))
	assert.Nil(t, f.locations.created, "an oversized body must not reach the store")
}

func TestMalformedJSONBodyIs422(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/locations", `{"name":`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "validation_failed", errorCode(t, rec))
}
