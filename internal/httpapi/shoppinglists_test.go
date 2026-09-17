package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeShoppingLists is an in-memory ShoppingListStore.
type fakeShoppingLists struct {
	list  *store.ShoppingList
	items []store.ShoppingListItem

	createErr  error
	getErr     error
	resolveErr error
	rematchErr error

	lastCreated  []store.NewShoppingListItem
	lastResolved uuid.UUID
	lastLine     *store.ResolveLine
	lastProduct  *uuid.UUID
	lastRawText  string
	resolves     int
	// catalog holds the rows FindCatalogProduct can return, by name.
	catalog     map[string]*store.CatalogProduct
	lastStorage uuid.UUID
}

func (f *fakeShoppingLists) CreateShoppingList(_ context.Context, storageID uuid.UUID, source store.ShoppingListSource, createdBy *uuid.UUID, items []store.NewShoppingListItem) (*store.ShoppingList, []store.ShoppingListItem, error) {
	f.lastStorage = storageID
	f.lastCreated = items
	if f.createErr != nil {
		return nil, nil, f.createErr
	}

	listID := uuid.New()
	list := &store.ShoppingList{ID: listID, StorageID: storageID, Source: source, CreatedBy: createdBy, CreatedAt: time.Now()}
	out := make([]store.ShoppingListItem, 0, len(items))
	for _, in := range items {
		out = append(out, store.ShoppingListItem{
			ID: uuid.New(), ShoppingListID: listID, RawText: in.RawText,
			Status: in.Status, MatchedProductID: in.MatchedProductID,
		})
	}
	f.list, f.items = list, out
	return list, out, nil
}

func (f *fakeShoppingLists) ShoppingListWithItems(_ context.Context, storageID, _ uuid.UUID) (*store.ShoppingList, []store.ShoppingListItem, error) {
	f.lastStorage = storageID
	if f.getErr != nil {
		return nil, nil, f.getErr
	}
	return f.list, f.items, nil
}

func (f *fakeShoppingLists) ShoppingListItemByID(_ context.Context, _, itemID uuid.UUID) (*store.ShoppingListItem, error) {
	for _, item := range f.items {
		if item.ID == itemID {
			return &item, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeShoppingLists) RematchShoppingListItem(_ context.Context, storageID, itemID uuid.UUID, rawText string, status store.ShoppingListItemStatus, matched *uuid.UUID) (*store.ShoppingListItem, error) {
	f.lastStorage, f.lastRawText, f.lastProduct = storageID, rawText, matched
	if f.rematchErr != nil {
		return nil, f.rematchErr
	}
	return &store.ShoppingListItem{ID: itemID, RawText: rawText, Status: status, MatchedProductID: matched}, nil
}

func (f *fakeShoppingLists) ResolveShoppingListItem(_ context.Context, storageID, itemID uuid.UUID, in store.ResolveLine, _ *uuid.UUID) (*store.ResolveResult, error) {
	f.resolves++
	f.lastStorage, f.lastResolved, f.lastLine, f.lastProduct = storageID, itemID, &in, in.ProductID
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}

	result := &store.ResolveResult{ProductCreated: in.NewProduct != nil}
	product := in.ProductID
	if in.NewProduct != nil {
		created := uuid.New()
		product = &created
	}
	if in.Quantity > 0 {
		batch := uuid.New()
		result.BatchID = &batch
	}
	quantity := in.Quantity
	result.Item = store.ShoppingListItem{
		ID: itemID, Status: store.ItemResolved, MatchedProductID: product, ResolvedQuantity: &quantity,
	}
	return result, nil
}

func (f *fakeShoppingLists) FindCatalogProduct(_ context.Context, name string) (*store.CatalogProduct, error) {
	if row, ok := f.catalog[store.NormalizeCatalogName(name)]; ok {
		return row, nil
	}
	return nil, store.ErrNotFound
}

// seedLine adds one unresolved line the resolve route can find, and returns
// its resolve path.
func (f *apiFixture) seedLine(rawText string, status store.ShoppingListItemStatus) (store.ShoppingListItem, string) {
	listID := uuid.New()
	item := store.ShoppingListItem{ID: uuid.New(), ShoppingListID: listID, RawText: rawText, Status: status}
	f.lists.items = append(f.lists.items, item)
	return item, f.base() + "/shopping-lists/" + listID.String() + "/items/" + item.ID.String() + "/resolve"
}

// fakeMatcher returns a canned match result.
type fakeMatcher struct {
	result matching.Result
	err    error
	calls  int
	texts  []string
}

func (f *fakeMatcher) MatchProductCandidates(_ context.Context, _ uuid.UUID, text string) (matching.Result, error) {
	f.calls++
	f.texts = append(f.texts, text)
	if f.err != nil {
		return matching.Result{}, f.err
	}
	result := f.result
	result.Query = text
	return result, nil
}

// TestCatalogResponsesCarryDisplayFieldsOnly is the privacy invariant on the
// wire. A catalog row describes what a product *is*; a response that leaked its
// id, its timestamps, or any count would tell a user that other households
// exist and that one of them wrote this description.
func TestCatalogResponsesCarryDisplayFieldsOnly(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	catalogID := uuid.New()
	variantID := uuid.New()
	category := "Food > Produce"
	shelfLife := 14

	f.matcher.result = matching.Result{
		Status: matching.StatusNewItem,
		Catalog: &matching.CatalogMatch{
			ID:                   catalogID,
			DisplayName:          "Tomatoes",
			CategoryPath:         &category,
			ItemType:             "perishable",
			DefaultShelfLifeDays: &shelfLife,
			Variants:             []matching.CatalogVariant{{ID: variantID, DisplayName: "Cherry Tomatoes"}},
		},
	}

	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"source":"text","raw_text":"tomatoes"}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	body := rec.Body.String()

	// The useful part is present. Asserted on the decoded value rather than
	// the raw bytes because Go's JSON encoder escapes ">" to > — which a
	// category path like "Food > Produce" is full of, and which any JSON
	// parser decodes back identically.
	var decoded struct {
		Items []struct {
			Catalog struct {
				DisplayName  string   `json:"display_name"`
				CategoryPath string   `json:"category_path"`
				Variants     []string `json:"variants"`
			} `json:"catalog"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded))
	require.Len(t, decoded.Items, 1)
	assert.Equal(t, "Tomatoes", decoded.Items[0].Catalog.DisplayName)
	assert.Equal(t, "Food > Produce", decoded.Items[0].Catalog.CategoryPath)
	assert.Equal(t, []string{"Cherry Tomatoes"}, decoded.Items[0].Catalog.Variants)

	// ...and every identifier and timestamp of the catalog row is not.
	assert.NotContains(t, body, catalogID.String(), "the catalog row's id must never reach a client")
	assert.NotContains(t, body, variantID.String(), "nor a variant's")
	assert.NotContains(t, body, "created_at\":\"0001", "no catalog timestamp")
	assert.NotContains(t, body, "storage_id")
	assert.NotContains(t, body, "normalized_name",
		"the uniqueness key is an internal detail of how rows are matched")
}

// TestNeedsImageSearchTracksTheStagedLookup covers the wire-level signal that
// carries the spec's "a line already described in catalog_products never
// triggers a Gemini or SerpAPI request" rule out to the client.
//
// It matters more than it looks. The image-suggestions endpoint has no
// matching awareness at all — it will call the providers for any query string
// it is handed — so this field is the only thing telling the frontend not to
// ask. Hardcoding it either way, or deriving it from status alone, would pass
// every other test in the suite while quietly spending an API call on a
// product the catalog already describes.
func TestNeedsImageSearchTracksTheStagedLookup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result matching.Result
		want   bool
	}{
		{
			name: "a catalog hit needs no external lookup",
			result: matching.Result{
				Status:  matching.StatusNewItem,
				Catalog: &matching.CatalogMatch{DisplayName: "Tomatoes"},
			},
			want: false,
		},
		{
			name:   "a genuine stage-3 miss is the only case that does",
			result: matching.Result{Status: matching.StatusNewItem},
			want:   true,
		},
		{
			name: "an exact local match never does",
			result: matching.Result{
				Status:  matching.StatusExactMatch,
				Product: &matching.LocalCandidate{ProductID: uuid.New(), Name: "Whole Milk", Similarity: 0.95},
			},
			want: false,
		},
		{
			name: "nor does an ambiguous one",
			result: matching.Result{
				Status:     matching.StatusAmbiguous,
				Candidates: []matching.LocalCandidate{{ProductID: uuid.New(), Name: "Cream", Similarity: 0.5}},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			f.matcher.result = tc.result

			rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"raw_text":"something"}`)
			require.Equal(t, http.StatusCreated, rec.Code)

			var body struct {
				Items []struct {
					NeedsImageSearch bool `json:"needs_image_search"`
				} `json:"items"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Len(t, body.Items, 1)

			assert.Equal(t, tc.want, body.Items[0].NeedsImageSearch)
		})
	}
}

// TestVariantsAreNamesNotObjects keeps the same rule for the alternatives: a
// variant is offered as something to click, not as a catalog handle.
func TestVariantsAreNamesNotObjects(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{
		Status: matching.StatusNewItem,
		Catalog: &matching.CatalogMatch{
			DisplayName: "Tomatoes",
			Variants: []matching.CatalogVariant{
				{ID: uuid.New(), DisplayName: "Cherry Tomatoes"},
				{ID: uuid.New(), DisplayName: "Yellow Tomatoes"},
			},
		},
	}

	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"raw_text":"tomatoes"}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	var body struct {
		Items []struct {
			Catalog struct {
				Variants []json.RawMessage `json:"variants"`
			} `json:"catalog"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	require.Len(t, body.Items[0].Catalog.Variants, 2)

	for _, variant := range body.Items[0].Catalog.Variants {
		var name string
		require.NoError(t, json.Unmarshal(variant, &name),
			"a variant must serialize as a bare display name, not an object with an id in it")
		assert.NotEmpty(t, name)
	}
}

// TestEveryLineBecomesItsOwnItem is the spec's first acceptance criterion at
// the HTTP boundary.
func TestEveryLineBecomesItsOwnItem(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/shopping-lists",
		`{"source":"text","raw_text":"milk\n\neggs x2\nbread\n"}`)

	require.Equal(t, http.StatusCreated, rec.Code)

	var body struct {
		Items []struct {
			RawText  string `json:"raw_text"`
			Quantity int    `json:"quantity"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Len(t, body.Items, 3, "blank lines are dropped, real lines each get a row")
	assert.Equal(t, "milk", body.Items[0].RawText)
	assert.Equal(t, 1, body.Items[0].Quantity)
	assert.Equal(t, "eggs x2", body.Items[1].RawText)
	assert.Equal(t, 2, body.Items[1].Quantity, "the parsed multiplier reaches the UI")
}

// TestMatchingSeesTheLineWithoutItsMultiplier — matching "eggs x2" against a
// product named "Eggs" would be dragged down by characters that were never
// part of the name.
func TestMatchingSeesTheLineWithoutItsMultiplier(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"raw_text":"eggs x2"}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, []string{"eggs"}, f.matcher.texts)
}

// TestPhotoListsAreRefusedExplicitly — treating an image as text would file its
// bytes as somebody's shopping.
func TestPhotoListsAreRefusedExplicitly(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"source":"photo"}`)

	require.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Equal(t, "not_implemented", errorCode(t, rec))
	assert.Zero(t, f.lists.lastCreated, "nothing may be written for a source we cannot process")
}

func TestShoppingListValidatesItsInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"no lines at all", `{"raw_text":""}`, "raw_text"},
		{"only blank lines", `{"raw_text":"\n   \n"}`, "raw_text"},
		{"an unknown source", `{"source":"telepathy","raw_text":"milk"}`, "source"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/shopping-lists", tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)[tc.wantField])
		})
	}
}

// TestNothingIsWrittenWhenMatchingFails — a half-matched list would present the
// user with statuses that were never actually decided.
func TestNothingIsWrittenWhenMatchingFails(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.err = errors.New("database down")

	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"raw_text":"milk\neggs"}`)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Nil(t, f.lists.lastCreated, "the list is written only after every line has been classified")
}

// TestResolveIsTheOnlyThingThatCanFinishALine covers the last acceptance
// criterion's boundary: the resolve call is explicit, names one line, and
// carries the quantity the user confirmed.
func TestResolveIsTheOnlyThingThatCanFinishALine(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	item, path := f.seedLine("milk", store.ItemExactMatch)
	productID, locationID := uuid.New(), uuid.New()

	rec := f.do(http.MethodPost, path,
		`{"product_id":"`+productID.String()+`","quantity":3,"location_id":"`+locationID.String()+`"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, item.ID, f.lists.lastResolved, "exactly the line that was named")
	require.NotNil(t, f.lists.lastLine)
	assert.Equal(t, 3, f.lists.lastLine.Quantity)
	require.NotNil(t, f.lists.lastLine.ProductID)
	assert.Equal(t, productID, *f.lists.lastLine.ProductID)
	require.NotNil(t, f.lists.lastLine.LocationID)
	assert.Equal(t, locationID, *f.lists.lastLine.LocationID)
	assert.Equal(t, f.storageID, f.lists.lastStorage)
}

// TestResolveDefaultsToTheLinesOwnQuantity — "milk" on a list means one milk
// and "eggs x2" two; a line dismissed without a product adds no stock at all.
func TestResolveDefaultsToTheLinesOwnQuantity(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	productID, locationID := uuid.New().String(), uuid.New().String()

	for raw, want := range map[string]int{"milk": 1, "eggs x2": 2} {
		_, path := f.seedLine(raw, store.ItemExactMatch)
		rec := f.do(http.MethodPost, path, `{"product_id":"`+productID+`","location_id":"`+locationID+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, want, f.lists.lastLine.Quantity, raw)
	}

	_, path := f.seedLine("eggs x2", store.ItemNewItem)
	rec := f.do(http.MethodPost, path, `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Zero(t, f.lists.lastLine.Quantity, "a dismissal adds nothing, whatever the line said")
}

// TestResolvingTwiceIsAConflict — the second resolve would double whatever the
// first one created.
func TestResolvingTwiceIsAConflict(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.lists.resolveErr = store.ErrConflict
	_, path := f.seedLine("milk", store.ItemNewItem)

	rec := f.do(http.MethodPost, path, `{}`)

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.NotContains(t, rec.Body.String(), "store:", "the store's own error text is not UI copy")
}

// TestRematchIsExplicit is the spec's fourth acceptance criterion: matching
// does not re-run on its own once a list exists — the user asks for it.
func TestRematchIsExplicit(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{Status: matching.StatusNewItem}

	rec := f.do(http.MethodPost,
		f.base()+"/shopping-lists/"+uuid.New().String()+"/items/"+uuid.New().String()+"/rematch",
		`{"raw_text":"oat milk"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "oat milk", f.lists.lastRawText)
	assert.Equal(t, 1, f.matcher.calls, "matching runs because it was asked for, once")
}

// TestReadingAListDoesNotRewriteItsStatuses — recomputing display detail must
// never move a line from new_item to exact_match underneath somebody reviewing
// it.
func TestReadingAListDoesNotRewriteItsStatuses(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	listID := uuid.New()
	f.lists.list = &store.ShoppingList{ID: listID, StorageID: f.storageID, Source: store.SourceText}
	f.lists.items = []store.ShoppingListItem{
		{ID: uuid.New(), ShoppingListID: listID, RawText: "milk", Status: store.ItemNewItem},
	}

	// The catalog has since learned about this product, so a fresh match would
	// classify it differently from how it was stored.
	f.matcher.result = matching.Result{
		Status:  matching.StatusExactMatch,
		Product: &matching.LocalCandidate{ProductID: uuid.New(), Name: "Whole Milk", Similarity: 0.95},
	}

	rec := f.do(http.MethodGet, f.base()+"/shopping-lists/"+listID.String(), "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)

	assert.Equal(t, "new_item", body.Items[0].Status,
		"the stored decision stands; only the choices shown alongside it are recomputed")
}

// TestAResolvedLineCostsNoMatching keeps a finished list cheap to reopen, and
// makes the "display only" claim concrete: there is nothing left to choose.
func TestAResolvedLineCostsNoMatching(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	listID := uuid.New()
	f.lists.list = &store.ShoppingList{ID: listID, StorageID: f.storageID, Source: store.SourceText}
	f.lists.items = []store.ShoppingListItem{
		{ID: uuid.New(), ShoppingListID: listID, RawText: "milk", Status: store.ItemResolved},
	}

	rec := f.do(http.MethodGet, f.base()+"/shopping-lists/"+listID.String(), "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, f.matcher.calls, "a decided line has no alternatives to offer")
}

// TestShoppingListRoutesAreStorageScoped — a list from another storage is a
// 404 that looks exactly like one that never existed.
func TestShoppingListRoutesAreStorageScoped(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.lists.getErr = store.ErrNotFound

	rec := f.do(http.MethodGet, f.base()+"/shopping-lists/"+uuid.New().String(), "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}
