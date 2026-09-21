package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// The HTTP half of docs/specs/16-product-maintenance.md: the detail read, the
// full PATCH, the merge and the delete.
//
// What these check that the store tests cannot: the status codes, which are
// load-bearing in this project. A foreign id is a 404 and never a 403; a value
// the model would refuse is a 422 from the handler rather than a 500 from a
// database CHECK; an unknown field is refused rather than ignored.

// seedProduct puts one product in the fake store and returns it.
func seedProduct(f *apiFixture, name string) store.Product {
	p := store.Product{
		ID:        uuid.New(),
		StorageID: f.storageID,
		Name:      name,
		ItemType:  store.ItemLongShelfLife,
	}
	f.products.products = append(f.products.products, p)
	return p
}

func decodeDetail(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// TestGetProductReturnsTheDetailView — everything the edit screen reads, and
// nothing that is server-side only.
func TestGetProductReturnsTheDetailView(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	product := seedProduct(f, "Gruyère")
	catalogID := uuid.New()
	f.products.products[0].CatalogID = &catalogID
	f.products.currentStock = 7
	batchID := uuid.New()
	f.products.batches = []store.Batch{{
		ID: batchID, ProductID: product.ID, LocationID: uuid.New(), Quantity: 7,
		ExpirationSource: store.ExpirationDerived, CreatedAt: time.Now(),
	}}
	who := "Tizian"
	f.products.logs = []store.ProductLog{{
		ID: uuid.New(), BatchID: &batchID, ChangeQty: 7,
		Reason: store.ReasonPurchase, CreatedBy: &who, Timestamp: time.Now(),
	}}

	rec := f.do(http.MethodGet, f.base()+"/products/"+product.ID.String(), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := decodeDetail(t, rec.Body.Bytes())
	assert.Equal(t, "Gruyère", body["name"])
	assert.EqualValues(t, 7, body["current_stock"])
	assert.Len(t, body["batches"], 1)
	assert.Len(t, body["logs"], 1)
	assert.NotContains(t, body, "catalog_id", "catalog_id is server-side only (docs/specs/02-data-model.md)")
	assert.NotContains(t, body, "storage_id")
	assert.NotContains(t, body, "recomputed_batches", "a read moved no dates")
	assert.Equal(t, 50, f.products.lastLogLimit, `"recent" is a bounded ledger, not the whole one`)
}

// TestGetProductInAnotherStorageIs404 — the rule the whole project turns on.
func TestGetProductInAnotherStorageIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.getErr = store.ErrNotFound

	rec := f.do(http.MethodGet, f.base()+"/products/"+uuid.New().String(), "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestPatchProductRejectsUnknownFields: an unknown field names a change the
// client believes was made. Ignoring it would answer 200 to a request that did
// nothing (docs/specs/16-product-maintenance.md).
func TestPatchProductRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	product := seedProduct(f, "Penne")

	rec := f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(),
		`{"name":"Penne Rigate","is_admin":true}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, "validation_failed", errorCode(t, rec))
	assert.Zero(t, f.products.updateCalls, "a refused body must not reach the store")
}

// TestPatchProductValidatesEveryField: each of these would otherwise reach a
// database CHECK or a VARCHAR width and surface as a 500 the caller cannot act
// on.
func TestPatchProductValidatesEveryField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"empty name", `{"name":"   "}`, "name"},
		{"unknown item type", `{"item_type":"frozen"}`, "item_type"},
		{"negative min stock", `{"min_stock":-1}`, "min_stock"},
		{"negative shelf life", `{"default_shelf_life_days":-5}`, "default_shelf_life_days"},
		{"absurd shelf life", `{"default_shelf_life_days":40000}`, "default_shelf_life_days"},
		{"malformed category id", `{"category_id":"not-a-uuid"}`, "category_id"},
		{"empty icon name", `{"icon_name":"  "}`, "icon_name"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			product := seedProduct(f, "Penne")

			rec := f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(), tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, errorFields(t, rec), tc.field)
			assert.Zero(t, f.products.updateCalls, "nothing invalid reaches the store")
		})
	}
}

// TestPatchProductNameLengthIsCountedInRunes: VARCHAR(255) counts characters,
// so a 200-character Cyrillic name is 400 bytes and perfectly legal. Counting
// bytes here would refuse it with a 422 the database never asked for.
func TestPatchProductNameLengthIsCountedInRunes(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	product := seedProduct(f, "Penne")
	cyrillic := strings.Repeat("ж", 200)

	rec := f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(),
		`{"name":"`+cyrillic+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(),
		`{"name":"`+strings.Repeat("ж", 256)+`"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "name")
}

// TestPatchProductReportsRecomputedBatchesOnlyForShelfLife is the line
// docs/specs/16-product-maintenance.md draws: a shelf-life change is made *in
// order to* move dates, so the count is reported; re-filing into another
// category runs the identical cascade and says nothing, as spec 08 specifies.
func TestPatchProductReportsRecomputedBatchesOnlyForShelfLife(t *testing.T) {
	t.Parallel()

	t.Run("shelf life reports the count", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		product := seedProduct(f, "Penne")
		f.products.recomputed = 3

		rec := f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(),
			`{"default_shelf_life_days":365}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		body := decodeDetail(t, rec.Body.Bytes())
		assert.EqualValues(t, 3, body["recomputed_batches"])
		require.NotNil(t, f.products.lastPatch)
		assert.True(t, f.products.lastPatch.SetDefaultShelfLifeDays)
		require.NotNil(t, f.products.lastPatch.DefaultShelfLifeDays)
		assert.Equal(t, 365, *f.products.lastPatch.DefaultShelfLifeDays)
	})

	t.Run("a null shelf life is still a shelf-life change", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		product := seedProduct(f, "Penne")
		f.products.recomputed = 0

		rec := f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(),
			`{"default_shelf_life_days":null}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		body := decodeDetail(t, rec.Body.Bytes())
		require.Contains(t, body, "recomputed_batches",
			"zero recomputed batches is an answer, not an absent field")
		assert.EqualValues(t, 0, body["recomputed_batches"])
		require.NotNil(t, f.products.lastPatch)
		assert.True(t, f.products.lastPatch.SetDefaultShelfLifeDays)
		assert.Nil(t, f.products.lastPatch.DefaultShelfLifeDays, "null clears the override")
	})

	t.Run("re-filing is silent", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		product := seedProduct(f, "Penne")
		f.products.recomputed = 3
		categoryID := uuid.New()

		rec := f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(),
			`{"category_id":"`+categoryID.String()+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		body := decodeDetail(t, rec.Body.Bytes())
		assert.NotContains(t, body, "recomputed_batches")
		require.NotNil(t, f.products.lastPatch)
		assert.True(t, f.products.lastPatch.SetCategoryID)
		require.NotNil(t, f.products.lastPatch.CategoryID)
		assert.Equal(t, categoryID, *f.products.lastPatch.CategoryID)
	})
}

// TestPatchProductDistinguishesAbsentFromNull — the whole reason the patch
// carries Set* flags rather than bare pointers.
func TestPatchProductDistinguishesAbsentFromNull(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	product := seedProduct(f, "Penne")

	rec := f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(), `{"name":"Penne Rigate"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, f.products.lastPatch)
	assert.False(t, f.products.lastPatch.SetCategoryID, "an absent field changes nothing")
	assert.False(t, f.products.lastPatch.SetIconName)
	assert.False(t, f.products.lastPatch.SetDefaultShelfLifeDays)

	rec = f.do(http.MethodPatch, f.base()+"/products/"+product.ID.String(),
		`{"category_id":null,"icon_name":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, f.products.lastPatch.SetCategoryID, "an explicit null clears")
	assert.Nil(t, f.products.lastPatch.CategoryID)
	assert.True(t, f.products.lastPatch.SetIconName)
	assert.Nil(t, f.products.lastPatch.IconName)
}

// TestPatchProductForeignIDsAre404 — a product or a category belonging to
// another storage is indistinguishable from one that does not exist.
func TestPatchProductForeignIDsAre404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.updateErr = store.ErrNotFound

	rec := f.do(http.MethodPatch, f.base()+"/products/"+uuid.New().String(), `{"name":"Whatever"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestMergeRefusesSelfMergeBeforeLookingAnythingUp: 422, and the store is never
// called — the answer to "can I merge a product into itself" must not depend on
// whether the id exists.
func TestMergeRefusesSelfMergeBeforeLookingAnythingUp(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	product := seedProduct(f, "Penne")

	rec := f.do(http.MethodPost, f.base()+"/products/"+product.ID.String()+"/merge",
		`{"source_product_id":"`+product.ID.String()+`"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "source_product_id")
	assert.Zero(t, f.products.mergeCalls)
}

// TestMergeRejectsUnknownFieldsAndMalformedSource.
func TestMergeRejectsUnknownFieldsAndMalformedSource(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	product := seedProduct(f, "Penne")
	path := f.base() + "/products/" + product.ID.String() + "/merge"

	rec := f.do(http.MethodPost, path, `{"source_product_id":"`+uuid.New().String()+`","keep_name":true}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	rec = f.do(http.MethodPost, path, `{"source_product_id":"nope"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "source_product_id")

	assert.Zero(t, f.products.mergeCalls)
}

// TestMergeAnswersWithTheSurvivorAndItsCounts.
func TestMergeAnswersWithTheSurvivorAndItsCounts(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	survivor := seedProduct(f, "Barilla Penne")
	sourceID := uuid.New()
	f.products.movedBatches = 2
	f.products.recomputed = 1

	rec := f.do(http.MethodPost, f.base()+"/products/"+survivor.ID.String()+"/merge",
		`{"source_product_id":"`+sourceID.String()+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := decodeDetail(t, rec.Body.Bytes())
	assert.Equal(t, "Barilla Penne", body["name"], "the survivor's own description wins")
	assert.EqualValues(t, 2, body["moved_batches"])
	assert.EqualValues(t, 1, body["recomputed_batches"])
	require.NotNil(t, f.products.lastMergeSourceID)
	assert.Equal(t, sourceID, *f.products.lastMergeSourceID)
	assert.Equal(t, survivor.ID, f.products.lastProductID, "the URL names the survivor")
}

// TestMergeWithAForeignSourceIs404.
func TestMergeWithAForeignSourceIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	survivor := seedProduct(f, "Penne")
	f.products.mergeErr = store.ErrNotFound

	rec := f.do(http.MethodPost, f.base()+"/products/"+survivor.ID.String()+"/merge",
		`{"source_product_id":"`+uuid.New().String()+`"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestMergeRemovesTheSourcePictureOnlyWhenOrphaned — the file goes after the
// transaction, and only when the store says nothing references it any more.
func TestMergeRemovesTheSourcePictureOnlyWhenOrphaned(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	survivor := seedProduct(f, "Penne")
	f.pictures.files["orphan.jpg"] = []byte("bytes")
	f.products.orphanedImage = f.base() + "/product-images/orphan.jpg"

	rec := f.do(http.MethodPost, f.base()+"/products/"+survivor.ID.String()+"/merge",
		`{"source_product_id":"`+uuid.New().String()+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, f.pictures.files, "orphan.jpg")

	// A shared picture: the store reports nothing, so nothing is removed.
	f.pictures.files["shared.jpg"] = []byte("bytes")
	f.products.orphanedImage = ""
	rec = f.do(http.MethodPost, f.base()+"/products/"+survivor.ID.String()+"/merge",
		`{"source_product_id":"`+uuid.New().String()+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, f.pictures.files, "shared.jpg")
}

// TestDeleteProductAnswers204AndRemovesAnOrphanedPicture.
func TestDeleteProductAnswers204AndRemovesAnOrphanedPicture(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	product := seedProduct(f, "Penne")
	f.pictures.files["gone.jpg"] = []byte("bytes")
	f.products.orphanedImage = f.base() + "/product-images/gone.jpg"

	rec := f.do(http.MethodDelete, f.base()+"/products/"+product.ID.String(), "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, 1, f.products.deleteCalls)
	assert.NotContains(t, f.pictures.files, "gone.jpg")
}

// TestDeleteProductInAnotherStorageIs404.
func TestDeleteProductInAnotherStorageIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.products.deleteErr = store.ErrNotFound

	rec := f.do(http.MethodDelete, f.base()+"/products/"+uuid.New().String(), "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
}

// TestProductMaintenanceRoutesRefuseANonMember is the gate, checked on the new
// routes specifically: the whole maintenance surface sits behind
// RequireStorageMember, and a non-member sees the same 404 as for a storage
// that does not exist (docs/specs/03-auth-and-multi-tenancy.md).
func TestProductMaintenanceRoutesRefuseANonMember(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	product := seedProduct(f, "Penne")
	other := uuid.New() // a storage this session is not a member of
	base := "/api/storages/" + other.String() + "/products/" + product.ID.String()

	for _, call := range []struct {
		method, path, body string
	}{
		{http.MethodGet, base, ""},
		{http.MethodPatch, base, `{"name":"x"}`},
		{http.MethodPost, base + "/merge", `{"source_product_id":"` + uuid.New().String() + `"}`},
		{http.MethodDelete, base, ""},
	} {
		rec := f.do(call.method, call.path, call.body)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", call.method, call.path)
		assert.Equal(t, "not_found", errorCode(t, rec))
	}
	assert.Zero(t, f.products.updateCalls)
	assert.Zero(t, f.products.mergeCalls)
	assert.Zero(t, f.products.deleteCalls)
}
