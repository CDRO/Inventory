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
	lastQuantity int
	lastProduct  *uuid.UUID
	lastRawText  string
	lastStorage  uuid.UUID
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

func (f *fakeShoppingLists) ResolveShoppingListItem(_ context.Context, storageID, itemID uuid.UUID, productID *uuid.UUID, quantity int) (*store.ShoppingListItem, error) {
	f.lastStorage, f.lastResolved, f.lastProduct, f.lastQuantity = storageID, itemID, productID, quantity
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return &store.ShoppingListItem{
		ID: itemID, Status: store.ItemResolved, MatchedProductID: productID, ResolvedQuantity: &quantity,
	}, nil
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
	listID, itemID, productID := uuid.New(), uuid.New(), uuid.New()

	rec := f.do(http.MethodPost,
		f.base()+"/shopping-lists/"+listID.String()+"/items/"+itemID.String()+"/resolve",
		`{"product_id":"`+productID.String()+`","quantity":3}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, itemID, f.lists.lastResolved, "exactly the line that was named")
	assert.Equal(t, 3, f.lists.lastQuantity)
	require.NotNil(t, f.lists.lastProduct)
	assert.Equal(t, productID, *f.lists.lastProduct)
	assert.Equal(t, f.storageID, f.lists.lastStorage)
}

func TestResolveDefaultsToOneUnit(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost,
		f.base()+"/shopping-lists/"+uuid.New().String()+"/items/"+uuid.New().String()+"/resolve", `{}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, f.lists.lastQuantity, `"milk" on a list means one milk`)
}

// TestResolvingTwiceIsAConflict — the second resolve would double whatever the
// first one created.
func TestResolvingTwiceIsAConflict(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.lists.resolveErr = store.ErrConflict

	rec := f.do(http.MethodPost,
		f.base()+"/shopping-lists/"+uuid.New().String()+"/items/"+uuid.New().String()+"/resolve", `{}`)

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
