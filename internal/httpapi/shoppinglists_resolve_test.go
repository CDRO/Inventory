package httpapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/imagesearch"
	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
)

const providerPicture = "https://provider.test/tomatoes.jpg"

// cardFor makes the matcher describe a line as a new item with a catalog card.
func (f *apiFixture) cardFor(name string, variants ...matching.CatalogVariant) *matching.CatalogMatch {
	path := "Food > Vegetables"
	image := providerPicture
	card := &matching.CatalogMatch{
		ID: uuid.New(), DisplayName: name, CategoryPath: &path, ItemType: "perishable",
		ImageURL: &image, Variants: variants,
	}
	f.matcher.result = matching.Result{Status: matching.StatusNewItem, Catalog: card}
	return card
}

// TestAcceptingACardCopiesItWithoutTheClientNamingARow — "Add this" sends no
// catalog id (the client was never given one). The server matches the line
// again, and the product it creates is the card that was on screen, linked to
// that row, with its provider picture fetched and copied into this storage.
func TestAcceptingACardCopiesItWithoutTheClientNamingARow(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	card := f.cardFor("Tomatoes")
	f.imageData.data, f.imageData.contentType = []byte("\xFF\xD8\xFFtomato"), "image/jpeg"
	location := uuid.NewString()
	_, path := f.seedLine("thomatoes", store.ItemNewItem)

	rec := f.do(http.MethodPost, path, `{"new_product":{"from":"catalog"},"quantity":2,"location_id":"`+location+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	line := f.lists.lastLine
	require.NotNil(t, line.NewProduct)
	p := line.NewProduct
	assert.Equal(t, "Tomatoes", p.Name)
	assert.Equal(t, store.ItemPerishable, p.ItemType)
	require.NotNil(t, p.AcceptedCatalogID)
	assert.Equal(t, card.ID, *p.AcceptedCatalogID, "linked to the row the line's card showed")
	assert.Nil(t, p.Catalog, "accepting describes nothing new to the catalog")
	require.NotNil(t, p.CategoryPath)
	assert.Equal(t, "Food > Vegetables", *p.CategoryPath, "resolved against this storage's tree by the store")

	require.NotNil(t, p.ImageURL)
	prefix := f.base() + "/product-images/"
	require.True(t, strings.HasPrefix(*p.ImageURL, prefix), *p.ImageURL)
	assert.Equal(t, []byte("\xFF\xD8\xFFtomato"), f.pictures.files[strings.TrimPrefix(*p.ImageURL, prefix)])
	assert.Equal(t, []string{providerPicture}, f.imageData.fetched, "fetched once, from the catalog's provider URL")

	var body struct {
		ProductCreated bool       `json:"product_created"`
		BatchID        *uuid.UUID `json:"batch_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.ProductCreated)
	assert.NotNil(t, body.BatchID)
}

// TestAnUnreachablePictureDoesNotCostTheProduct — a card accepted while its
// provider is down still creates the product the person accepted, without a
// picture.
func TestAnUnreachablePictureDoesNotCostTheProduct(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.cardFor("Tomatoes")
	f.imageData.fetchErr = errors.New("provider unreachable")
	_, path := f.seedLine("tomatoes", store.ItemNewItem)

	rec := f.do(http.MethodPost, path, `{"new_product":{"from":"catalog"}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.NotNil(t, f.lists.lastLine.NewProduct)
	assert.Nil(t, f.lists.lastLine.NewProduct.ImageURL)
	assert.Empty(t, f.pictures.files)
}

// TestAcceptingAVariantTakesOnlyOneTheCardOffered — the variants are one-click
// alternatives to the card. A variant the card did not offer is refused, so a
// client cannot pull in a catalog description it was never shown.
func TestAcceptingAVariantTakesOnlyOneTheCardOffered(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	cherry := uuid.New()
	f.cardFor("Tomatoes", matching.CatalogVariant{ID: cherry, DisplayName: "Cherry Tomatoes"})
	path := "Food > Vegetables > Small"
	f.lists.catalog = map[string]*store.CatalogProduct{
		"cherry tomatoes": {ID: cherry, DisplayName: "Cherry Tomatoes", CategoryPath: &path, ItemType: store.ItemPerishable},
		"secret recipe":   {ID: uuid.New(), DisplayName: "Secret Recipe", ItemType: store.ItemPerishable},
	}

	_, offered := f.seedLine("tomatoes, c.", store.ItemNewItem)
	rec := f.do(http.MethodPost, offered, `{"new_product":{"from":"catalog","variant":"cherry tomatoes"}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	p := f.lists.lastLine.NewProduct
	assert.Equal(t, "Cherry Tomatoes", p.Name)
	require.NotNil(t, p.AcceptedCatalogID)
	assert.Equal(t, cherry, *p.AcceptedCatalogID)
	assert.Equal(t, "Food > Vegetables > Small", *p.CategoryPath)

	resolvesBefore := f.lists.resolves
	_, notOffered := f.seedLine("tomatoes, c.", store.ItemNewItem)
	rec = f.do(http.MethodPost, notOffered, `{"new_product":{"from":"catalog","variant":"Secret Recipe"}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.NotEmpty(t, errorFields(t, rec)["new_product.variant"])
	assert.Equal(t, resolvesBefore, f.lists.resolves, "nothing reaches the store")
}

// TestALineWithoutACardCannotAcceptOne — "Add this" only exists for a new item
// the catalog described.
func TestALineWithoutACardCannotAcceptOne(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{Status: matching.StatusNewItem}
	_, path := f.seedLine("smoked paprika", store.ItemNewItem)

	rec := f.do(http.MethodPost, path, `{"new_product":{"from":"catalog"}}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.NotEmpty(t, errorFields(t, rec)["new_product.from"])
	assert.Zero(t, f.lists.resolves)
}

// TestDescribingAProductPromotesItsPictureAndTellsTheCatalogOnlyTheSource —
// the stage-3 path. The picked suggestion becomes this product's permanent
// picture, and the catalog is told only where the provider picture came from:
// never this storage's copy, never the cache.
func TestDescribingAProductPromotesItsPictureAndTellsTheCatalogOnlyTheSource(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{Status: matching.StatusNewItem}
	hash := imagesearch.HashURL("https://provider.test/paprika.png")
	f.imageData.sources = map[string]string{hash: "https://provider.test/paprika.png"}
	f.imageData.data, f.imageData.contentType = []byte("\x89PNGpaprika"), "image/png"
	category := uuid.New()
	_, path := f.seedLine("smoked paprika", store.ItemNewItem)

	rec := f.do(http.MethodPost, path, `{"new_product":{"from":"manual","name":" Smoked Paprika ",
		"category_id":"`+category.String()+`","item_type":"long_shelf_life","min_stock":2,"image":"`+hash+`"},
		"quantity":1,"location_id":"`+uuid.NewString()+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	p := f.lists.lastLine.NewProduct
	require.NotNil(t, p)
	assert.Equal(t, "Smoked Paprika", p.Name)
	assert.Equal(t, 2, p.MinStock)
	require.NotNil(t, p.CategoryID)
	assert.Equal(t, category, *p.CategoryID)

	require.NotNil(t, p.ImageURL)
	prefix := f.base() + "/product-images/"
	require.True(t, strings.HasPrefix(*p.ImageURL, prefix), *p.ImageURL)
	assert.Equal(t, []byte("\x89PNGpaprika"), f.pictures.files[strings.TrimPrefix(*p.ImageURL, prefix)])

	require.NotNil(t, p.Catalog)
	assert.Equal(t, "Smoked Paprika", p.Catalog.DisplayName)
	require.NotNil(t, p.Catalog.ImageURL)
	assert.Equal(t, "https://provider.test/paprika.png", *p.Catalog.ImageURL)
	assert.Nil(t, p.Catalog.ShownID, "no card was shown, so there is nothing to be a variant of")
}

// TestDecliningTheCardMakesTheNewNameItsVariant — spec 07: "It's something
// else" with a different name links the new catalog row to the one shown. The
// same name is not a variant of itself.
func TestDecliningTheCardMakesTheNewNameItsVariant(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	card := f.cardFor("Tomatoes")

	_, differently := f.seedLine("tomatoes", store.ItemNewItem)
	rec := f.do(http.MethodPost, differently, `{"new_product":{"from":"manual","name":"Yellow Tomatoes"}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, f.lists.lastLine.NewProduct.Catalog.ShownID)
	assert.Equal(t, card.ID, *f.lists.lastLine.NewProduct.Catalog.ShownID)

	_, same := f.seedLine("tomatoes", store.ItemNewItem)
	rec = f.do(http.MethodPost, same, `{"new_product":{"from":"manual","name":"  tomatoes "}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Nil(t, f.lists.lastLine.NewProduct.Catalog.ShownID)
}

// TestAResolveThatFailsLeavesNoPictureBehind — a picture is copied before the
// transaction; a confirm the store then refuses removes it again.
func TestAResolveThatFailsLeavesNoPictureBehind(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{Status: matching.StatusNewItem}
	hash := imagesearch.HashURL("https://provider.test/x.png")
	f.imageData.sources = map[string]string{hash: "https://provider.test/x.png"}
	f.lists.resolveErr = store.ErrNotFound
	_, path := f.seedLine("oat milk", store.ItemNewItem)

	rec := f.do(http.MethodPost, path, `{"new_product":{"from":"manual","name":"Oat Milk","image":"`+hash+`"},
		"quantity":1,"location_id":"`+uuid.NewString()+`"}`)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Empty(t, f.pictures.files)
}

// TestAPictureNoLongerCachedIsAskedForAgain — a suggestion evicted between
// being shown and being confirmed is a 422 on the picture, not a product with
// a broken image, and nothing is written.
func TestAPictureNoLongerCachedIsAskedForAgain(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.matcher.result = matching.Result{Status: matching.StatusNewItem}
	_, path := f.seedLine("oat milk", store.ItemNewItem)

	rec := f.do(http.MethodPost, path, `{"new_product":{"from":"manual","name":"Oat Milk","image":"`+strings.Repeat("d", 64)+`"}}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.NotEmpty(t, errorFields(t, rec)["new_product.image"])
	assert.Zero(t, f.lists.resolves)
	assert.Empty(t, f.pictures.files)
}

func TestResolveValidatesItsShape(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	id := uuid.NewString()

	for name, tc := range map[string]struct{ body, field string }{
		"both product kinds":   {`{"product_id":"` + id + `","new_product":{"from":"manual","name":"x"}}`, "product_id"},
		"a negative quantity":  {`{"product_id":"` + id + `","quantity":-1}`, "quantity"},
		"an unknown source":    {`{"new_product":{"from":"guess","name":"x"}}`, "new_product.from"},
		"a nameless product":   {`{"new_product":{"from":"manual","name":"  "}}`, "new_product.name"},
		"an unknown item type": {`{"new_product":{"from":"manual","name":"x","item_type":"liquid"}}`, "new_product.item_type"},
		"a negative min stock": {`{"new_product":{"from":"manual","name":"x","min_stock":-1}}`, "new_product.min_stock"},
	} {
		_, path := f.seedLine("milk", store.ItemNewItem)
		rec := f.do(http.MethodPost, path, tc.body)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, name)
		assert.NotEmpty(t, errorFields(t, rec)[tc.field], name)
	}
	assert.Zero(t, f.lists.resolves, "nothing reaches the store while the body is invalid")
}

// TestACardNeverShowsTheProvidersAddress — the catalog records where a
// picture came from, and that address must never reach a browser: every card
// shows the picture from this origin instead.
func TestACardNeverShowsTheProvidersAddress(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.cardFor("Tomatoes")

	rec := f.do(http.MethodPost, f.base()+"/shopping-lists", `{"raw_text":"tomatoes"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "provider.test")

	var body struct {
		Items []struct {
			Catalog *struct {
				ImageURL *string `json:"image_url"`
			} `json:"catalog"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	require.NotNil(t, body.Items[0].Catalog)
	require.NotNil(t, body.Items[0].Catalog.ImageURL)
	assert.Equal(t, f.base()+"/images/"+imagesearch.HashURL(providerPicture), *body.Items[0].Catalog.ImageURL)

	// The same holds for the reorder dashboard's card.
	rec = f.do(http.MethodPost, f.base()+"/dashboard/reorder/items/match", `{"name":"tomatoes"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "provider.test")
	assert.Contains(t, rec.Body.String(), f.base()+"/images/")
}
