package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeReorderStore is an in-memory ReorderStore.
type fakeReorderStore struct {
	rows        []store.ReorderProduct
	rowsErr     error
	minStocks   map[uuid.UUID]int
	minStockErr error
	updateErr   error
	createErr   error
	updated     *store.Product
	created     *store.Product
	lastMin     int
	lastID      uuid.UUID
	lastNew     store.NewProduct
	createCalls int
	updateCalls int
}

func (f *fakeReorderStore) ReorderProducts(_ context.Context, _ uuid.UUID) ([]store.ReorderProduct, error) {
	return f.rows, f.rowsErr
}

func (f *fakeReorderStore) ProductMinStock(_ context.Context, _, id uuid.UUID) (int, error) {
	if f.minStockErr != nil {
		return 0, f.minStockErr
	}
	return f.minStocks[id], nil
}

func (f *fakeReorderStore) UpdateProductMinStockAsUser(_ context.Context, storageID, id uuid.UUID, minStock int, _ uuid.UUID) (*store.Product, error) {
	f.updateCalls++
	f.lastID, f.lastMin = id, minStock
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.updated != nil {
		return f.updated, nil
	}
	return &store.Product{ID: id, StorageID: storageID, Name: "Existing", MinStock: minStock}, nil
}

func (f *fakeReorderStore) CreateProduct(_ context.Context, storageID uuid.UUID, in store.NewProduct) (*store.Product, error) {
	f.createCalls++
	f.lastNew = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &store.Product{ID: uuid.New(), StorageID: storageID, Name: in.Name, MinStock: in.MinStock,
		CategoryID: in.CategoryID, ItemType: in.ItemType, ImageURL: in.ImageURL, IconName: in.IconName}, nil
}

// TestReorderDashboardExcludesZeroThreshold — a product with min_stock = 0 is
// "not tracked for reorder" and must never appear in either bucket, regardless
// of how empty its shelf is (docs/specs/10-reorder-and-shopping-export.md).
func TestReorderDashboardExcludesZeroThresholdAndClassifiesCorrectly(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	outID, lowID, untracked := uuid.New(), uuid.New(), uuid.New()
	f.reorder.rows = []store.ReorderProduct{
		{ProductID: outID, Name: "Butter", CurrentStock: 0, MinStock: 2},
		{ProductID: lowID, Name: "Milk", CurrentStock: 1, MinStock: 3},
		// A row with min_stock = 0 must never be produced by the store query in
		// the first place; it is included here anyway to prove the handler
		// would still exclude it even if the query ever regressed.
		{ProductID: untracked, Name: "Salt", CurrentStock: 0, MinStock: 0},
	}

	rec := f.do(http.MethodGet, f.base()+"/dashboard/reorder", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		OutOfStock []struct {
			ProductID string `json:"product_id"`
			Name      string `json:"name"`
			MinStock  int    `json:"min_stock"`
		} `json:"out_of_stock"`
		LowStock []struct {
			ProductID    string `json:"product_id"`
			Name         string `json:"name"`
			CurrentStock int    `json:"current_stock"`
			MinStock     int    `json:"min_stock"`
		} `json:"low_stock"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Len(t, body.OutOfStock, 1)
	assert.Equal(t, outID.String(), body.OutOfStock[0].ProductID)
	assert.Equal(t, 2, body.OutOfStock[0].MinStock)

	require.Len(t, body.LowStock, 1)
	assert.Equal(t, lowID.String(), body.LowStock[0].ProductID)
	assert.Equal(t, 1, body.LowStock[0].CurrentStock)
	assert.Equal(t, 3, body.LowStock[0].MinStock)

	assert.NotContains(t, rec.Body.String(), untracked.String())
}

// TestReorderExportSharesTheDashboardClassification pins the acceptance
// criterion that the CSV export and the dashboard read the same rows and
// apply the same low/out-of-stock split — the same fixture rows a dashboard
// test uses, checked against the CSV instead of the JSON.
func TestReorderExportSharesTheDashboardClassification(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.reorder.rows = []store.ReorderProduct{
		{ProductID: uuid.New(), Name: "Butter", CurrentStock: 0, MinStock: 2},
		{ProductID: uuid.New(), Name: "Milk", CurrentStock: 1, MinStock: 3},
	}

	rec := f.do(http.MethodGet, f.base()+"/dashboard/reorder/export?format=csv", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "text/csv; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment; filename=\"reorder-list-")

	body := rec.Body.String()
	assert.Contains(t, body, "Butter,0,2,2")
	assert.Contains(t, body, "Milk,1,3,2")
}

// TestReorderExportRejectsPDF — the spec is explicit that a server-side PDF
// endpoint is deliberately not built; PDF is generated client-side from the
// same JSON the dashboard fetched.
func TestReorderExportRejectsPDF(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/dashboard/reorder/export?format=pdf", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestReorderAddItemCreatesNoBatchesOrLogs is the central acceptance
// criterion of this issue: adding a missing product creates a products row
// with zero batches and no inventory_logs row — the store fake only exposes
// CreateProduct, so a batch or log write is not merely unrecorded here, it is
// structurally impossible for this handler to perform one.
func TestReorderAddItemCreatesProductWithNoBatchesAndDefaultsMinStockToOne(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{Status: matching.StatusNewItem}

	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items", `{"name":"Butter"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	assert.Equal(t, 1, f.reorder.createCalls, "must create exactly one product")
	assert.Equal(t, 0, f.reorder.updateCalls, "must not go through the update-existing path")
	assert.Equal(t, "Butter", f.reorder.lastNew.Name)
	assert.Equal(t, 1, f.reorder.lastNew.MinStock, "min_stock must default to 1, never 0")

	var body struct {
		MinStock int  `json:"min_stock"`
		Created  bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 1, body.MinStock)
	assert.True(t, body.Created)
}

// TestReorderAddItemRejectsExplicitZeroMinStock — a product created here with
// min_stock = 0 would vanish from the very list it was created on, so the
// spec's floor of 1 is enforced even when a caller asks for 0 explicitly, not
// only when the field is omitted.
func TestReorderAddItemRejectsExplicitZeroMinStock(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items", `{"name":"Butter","min_stock":0}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, 0, f.reorder.createCalls)
}

// TestReorderAddItemWithExistingNameAdjustsMinStockInsteadOfDuplicating is the
// other half of the central acceptance criterion: a name that already exists
// in the storage must adjust that product's min_stock, never create a second
// product with the same name — enforced server-side via the matcher, not just
// by a well-behaved client sending product_id.
func TestReorderAddItemWithExistingNameAdjustsMinStockInsteadOfDuplicating(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	existingID := uuid.New()
	f.matcher.result = matching.Result{
		Status:  matching.StatusExactMatch,
		Product: &matching.LocalCandidate{ProductID: existingID, Name: "Butter", Similarity: 1},
	}

	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items", `{"name":"butter","min_stock":2}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, 0, f.reorder.createCalls, "must not create a second product with the same name")
	assert.Equal(t, 1, f.reorder.updateCalls)
	assert.Equal(t, existingID, f.reorder.lastID)
	assert.Equal(t, 2, f.reorder.lastMin)

	var body struct {
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.False(t, body.Created)
}

// TestReorderAddItemTrustsExplicitProductID — a caller that already
// disambiguated (picked one of several "ambiguous" candidates, or a prior
// exact match) can name the product directly; the handler must not re-run
// matching and must not create a duplicate.
func TestReorderAddItemTrustsExplicitProductID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	existingID := uuid.New()

	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items",
		`{"name":"Butter","product_id":"`+existingID.String()+`","min_stock":5}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, 0, f.matcher.calls, "an explicit product_id must not trigger a re-match")
	assert.Equal(t, 0, f.reorder.createCalls)
	assert.Equal(t, 1, f.reorder.updateCalls)
	assert.Equal(t, existingID, f.reorder.lastID)
	assert.Equal(t, 5, f.reorder.lastMin)
}

// TestReorderMatchNeverCallsExternalSearchOnCatalogHit is the acceptance
// criterion "the add-item flow issues no external API call when the catalog
// already describes the product" pinned at the point that decides it:
// NeedsExternalLookup must be false whenever a catalog hit is present.
func TestReorderMatchNeverCallsExternalSearchOnCatalogHit(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{
		Status: matching.StatusNewItem,
		Catalog: &matching.CatalogMatch{
			ID:          uuid.New(),
			DisplayName: "Salted Butter",
		},
	}

	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items/match", `{"name":"butter"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Status           string `json:"status"`
		NeedsImageSearch bool   `json:"needs_image_search"`
		Catalog          *struct {
			DisplayName string `json:"display_name"`
		} `json:"catalog"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "new_item", body.Status)
	require.NotNil(t, body.Catalog)
	assert.Equal(t, "Salted Butter", body.Catalog.DisplayName)
	assert.False(t, body.NeedsImageSearch)
}

// TestReorderMatchFlagsExternalSearchOnFullMiss is the complementary case: a
// genuine miss (no local product, no catalog row) is the only situation an
// image search is worth its cost.
func TestReorderMatchFlagsExternalSearchOnFullMiss(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{Status: matching.StatusNewItem}

	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items/match", `{"name":"unobtainium"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"needs_image_search":true`)
}

// TestReorderMatchReportsTheMatchedProductsActualMinStock guards a real
// overwrite risk: a matched product can be a confident local match while
// sitting well outside both dashboard buckets (already well-stocked, so
// ReorderProducts never surfaces it), so the confirm UI cannot get its
// current threshold from anywhere else. If Match answered a placeholder
// instead of the real value, confirming a match without editing the
// prefilled field would silently drop an existing high min_stock to 1.
func TestReorderMatchReportsTheMatchedProductsActualMinStock(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	existingID := uuid.New()
	f.matcher.result = matching.Result{
		Status:  matching.StatusExactMatch,
		Product: &matching.LocalCandidate{ProductID: existingID, Name: "Milk", Similarity: 1},
	}
	f.reorder.minStocks = map[uuid.UUID]int{existingID: 5}

	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items/match", `{"name":"milk"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		MatchedProduct struct {
			MinStock int `json:"min_stock"`
		} `json:"matched_product"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 5, body.MatchedProduct.MinStock, "must be the product's real threshold, not a placeholder")
}

// TestReorderMatchReportsMinStockForAmbiguousCandidates is the same guard for
// the "which one did you mean?" state: whichever candidate the user picks
// goes through the same confirm step, so each candidate needs its own real
// min_stock too.
func TestReorderMatchReportsMinStockForAmbiguousCandidates(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	first, second := uuid.New(), uuid.New()
	f.matcher.result = matching.Result{
		Status: matching.StatusAmbiguous,
		Candidates: []matching.LocalCandidate{
			{ProductID: first, Name: "Oat milk", Similarity: 0.5},
			{ProductID: second, Name: "Almond milk", Similarity: 0.45},
		},
	}
	f.reorder.minStocks = map[uuid.UUID]int{first: 2, second: 0}

	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items/match", `{"name":"milk"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Candidates []struct {
			ID       string `json:"id"`
			MinStock int    `json:"min_stock"`
		} `json:"candidates"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Candidates, 2)
	byID := map[string]int{body.Candidates[0].ID: body.Candidates[0].MinStock, body.Candidates[1].ID: body.Candidates[1].MinStock}
	assert.Equal(t, 2, byID[first.String()])
	assert.Equal(t, 0, byID[second.String()])
}

// TestReorderMatchRejectsBlankName mirrors the same validation every other
// free-text entry point in this package applies.
func TestReorderMatchRejectsBlankName(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items/match", `{"name":"   "}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestReorderAddItemRejectsMalformedProductID checks the body-field validation
// path (distinct from the path-parameter 404-not-403 rule): a malformed
// product_id in a JSON body is a 422, the same as any other unusable payload.
func TestReorderAddItemRejectsMalformedProductID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items", `{"name":"Butter","product_id":"not-a-uuid"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestReorderAddItemUpdateOfForeignProductIs404 confirms the store's
// ErrNotFound (a product in another storage, or none at all) surfaces as the
// ordinary 404 rather than anything more specific.
func TestReorderAddItemUpdateOfForeignProductIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.reorder.updateErr = store.ErrNotFound

	rec := f.do(http.MethodPost, f.base()+"/dashboard/reorder/items",
		`{"name":"Butter","product_id":"`+uuid.NewString()+`","min_stock":1}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestReorderRoutesAbsentWithoutMatcher documents the same "absent
// collaborator, absent route" rule the shopping-list group already follows:
// the dashboard and export need no matcher and stay up, but the two routes
// that depend on it (Match, AddItem) must not be registered at all when
// Deps.Matcher is nil, rather than registered and failing on every call.
func TestReorderRoutesAbsentWithoutMatcher(t *testing.T) {
	t.Parallel()

	auth := newFakeAuth()
	user, session := auth.addUser(t, false)
	storageID := uuid.New()
	auth.addMember(storageID, user.ID)

	router := httpapi.NewRouter(httpapi.Deps{
		Errors: httpapi.NewErrorWriter(false, discardLogger()),
		Store: fakeAPI{
			fakeAuth: auth, fakeLocations: &fakeLocations{}, fakeBatches: &fakeBatches{},
			fakeShoppingLists: &fakeShoppingLists{}, fakeExpiry: &fakeExpiry{},
			fakeJobs: newFakeJobs(), fakeIdempotency: newFakeIdempotency(), fakeIngestStore: &fakeIngestStore{},
			fakeConsumeStore: &fakeConsumeStore{}, fakeProductStore: &fakeProductStore{},
			fakeReorderStore: &fakeReorderStore{},
		},
		// Matcher deliberately omitted (nil).
	})

	req := httptest.NewRequest(http.MethodPost, "/api/storages/"+storageID.String()+"/dashboard/reorder/items", strings.NewReader(`{"name":"x"}`))
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: session.ID})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// The dashboard route has no such dependency and must still work.
	getReq := httptest.NewRequest(http.MethodGet, "/api/storages/"+storageID.String()+"/dashboard/reorder", nil)
	getReq.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: session.ID})
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	assert.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
}
