package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// resolveFixture is a storage with one shelf, one product, and a list whose
// lines are all new items. The matching outcome does not matter to the store,
// which applies whatever resolution it is handed.
type resolveFixture struct {
	storageID uuid.UUID
	userID    uuid.UUID
	pantry    uuid.UUID
	milk      uuid.UUID
	items     []store.ShoppingListItem
}

func newResolveFixture(t *testing.T, s *store.Store, texts ...string) resolveFixture {
	t.Helper()
	ctx := context.Background()
	f := resolveFixture{storageID: newStorage(t, ctx), userID: newUser(t, ctx)}

	pantry, err := s.CreateLocation(ctx, f.storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	f.pantry = pantry.ID

	milk, err := s.CreateProduct(ctx, f.storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)
	f.milk = milk.ID

	_, f.items, err = s.CreateShoppingList(ctx, f.storageID, store.SourceText, &f.userID, lines(texts...))
	require.NoError(t, err)
	return f
}

func (f resolveFixture) productCount(t *testing.T) int {
	t.Helper()
	return countRows(t, context.Background(), `SELECT count(*) FROM products WHERE storage_id = $1`, f.storageID)
}

func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }

// TestResolvingAMatchAddsTheStockItConfirmed is the exact-match path of spec
// 07's "Resolution UI per state": the confirmed quantity becomes one batch at
// the chosen location, paired with a purchase log row in the same transaction
// (CLAUDE.md, "Invariants that fail silently").
func TestResolvingAMatchAddsTheStockItConfirmed(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newResolveFixture(t, s, "milk x3")

	result, err := s.ResolveShoppingListItem(ctx, f.storageID, f.items[0].ID, store.ResolveLine{
		ProductID: &f.milk, Quantity: 3, LocationID: &f.pantry,
	}, &f.userID)
	require.NoError(t, err)

	assert.False(t, result.ProductCreated)
	require.NotNil(t, result.BatchID)
	assert.Equal(t, store.ItemResolved, result.Item.Status)
	require.NotNil(t, result.Item.MatchedProductID)
	assert.Equal(t, f.milk, *result.Item.MatchedProductID)
	require.NotNil(t, result.Item.ResolvedQuantity)
	assert.Equal(t, 3, *result.Item.ResolvedQuantity)

	assert.Equal(t, 1, countRows(t, ctx, `
		SELECT count(*) FROM inventory_batches
		 WHERE id = $1 AND product_id = $2 AND location_id = $3 AND quantity = 3`,
		*result.BatchID, f.milk, f.pantry))
	assert.Equal(t, 1, countRows(t, ctx, `
		SELECT count(*) FROM inventory_logs
		 WHERE batch_id = $1 AND change_qty = 3 AND reason = 'purchase' AND created_by = $2`,
		*result.BatchID, f.userID), "the batch is paired with its purchase log row")
}

// TestResolvingANewItemCreatesTheProductAndDescribesItToTheCatalog is the
// no-catalog-hit path: the product, its catalog row (with the category path,
// never an id) and its first batch, all in one confirm.
func TestResolvingANewItemCreatesTheProductAndDescribesItToTheCatalog(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newResolveFixture(t, s, "smoked paprika")

	food, err := s.CreateCategory(ctx, f.storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	spices, err := s.CreateCategory(ctx, f.storageID, store.NewCategory{Name: "Spices", ParentID: &food.ID})
	require.NoError(t, err)

	name := "Smoked Paprika " + uuid.NewString()[:8]
	source := "https://example.test/paprika.jpg"
	picture := "/api/storages/" + f.storageID.String() + "/product-images/" + uuid.Must(uuid.NewV7()).String() + ".jpg"

	result, err := s.ResolveShoppingListItem(ctx, f.storageID, f.items[0].ID, store.ResolveLine{
		NewProduct: &store.ResolvedProduct{
			Name: name, ItemType: store.ItemLongShelfLife, MinStock: 1, CategoryID: &spices.ID,
			ImageURL: &picture,
			Catalog:  &store.NewCatalogProduct{DisplayName: name, ItemType: store.ItemLongShelfLife, ImageURL: &source},
		},
		Quantity: 2, LocationID: &f.pantry,
	}, &f.userID)
	require.NoError(t, err)

	assert.True(t, result.ProductCreated)
	require.NotNil(t, result.BatchID)

	var productImage, catalogImage, categoryPath *string
	var minStock int
	require.NoError(t, testPool.QueryRow(ctx, `
		SELECT p.image_url, p.min_stock, c.image_url, c.category_path
		  FROM products p JOIN catalog_products c ON c.id = p.catalog_id
		 WHERE p.id = $1 AND p.storage_id = $2 AND p.category_id = $3`,
		*result.Item.MatchedProductID, f.storageID, spices.ID).Scan(&productImage, &minStock, &catalogImage, &categoryPath))

	require.NotNil(t, productImage)
	assert.Equal(t, picture, *productImage, "the product keeps its own permanent picture")
	require.NotNil(t, catalogImage)
	assert.Equal(t, source, *catalogImage, "the catalog records the provider image, never this storage's URL")
	require.NotNil(t, categoryPath)
	assert.Equal(t, "Food > Spices", *categoryPath, "a path of names, never this storage's category ids")
	assert.Equal(t, 1, minStock)
}

// TestAcceptingACatalogCardCreatesMissingCategoriesAndLinksTheRow is "Add
// this" on a catalog card: it copies the card into a storage-local product,
// resolves its category path against this storage's tree creating what is
// missing, and adds nothing to the catalog, because the row already describes
// the product.
func TestAcceptingACatalogCardCreatesMissingCategoriesAndLinksTheRow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newResolveFixture(t, s, "tomatoes")

	// "Food" exists here with different case; "Vegetables" does not exist.
	_, err := s.CreateCategory(ctx, f.storageID, store.NewCategory{Name: "food"})
	require.NoError(t, err)

	path := "Food > Vegetables"
	card, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Card Tomatoes " + uuid.NewString()[:8], CategoryPath: &path, ItemType: store.ItemPerishable,
	})
	require.NoError(t, err)
	catalogBefore := countRows(t, ctx, `SELECT count(*) FROM catalog_products`)

	result, err := s.ResolveShoppingListItem(ctx, f.storageID, f.items[0].ID, store.ResolveLine{
		NewProduct: &store.ResolvedProduct{
			Name: card.DisplayName, ItemType: card.ItemType, CategoryPath: card.CategoryPath,
			AcceptedCatalogID: &card.ID,
		},
		Quantity: 1, LocationID: &f.pantry,
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, catalogBefore, countRows(t, ctx, `SELECT count(*) FROM catalog_products`), "accepting adds no catalog row")
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM products WHERE id = $1 AND catalog_id = $2`, *result.Item.MatchedProductID, card.ID))

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM categories WHERE storage_id = $1 AND lower(name) = 'food'`, f.storageID),
		"an existing node is reused, whatever its case")
	assert.Equal(t, 1, countRows(t, ctx, `
		SELECT count(*) FROM products p
		  JOIN categories leaf ON leaf.id = p.category_id AND leaf.name = 'Vegetables'
		  JOIN categories parent ON parent.id = leaf.parent_id AND lower(parent.name) = 'food'
		 WHERE p.id = $1`, *result.Item.MatchedProductID), "the missing node is created under it")
}

// TestDecliningACardLinksTheNewNameAsAVariant: spec 07 makes declining the
// card that was shown, and naming the product differently, the only way the
// variant graph grows.
func TestDecliningACardLinksTheNewNameAsAVariant(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newResolveFixture(t, s, "tomatoes, c.")

	shown, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{DisplayName: "Shown Tomatoes " + uuid.NewString()[:8]})
	require.NoError(t, err)
	name := "Cherry Tomatoes " + uuid.NewString()[:8]

	result, err := s.ResolveShoppingListItem(ctx, f.storageID, f.items[0].ID, store.ResolveLine{
		NewProduct: &store.ResolvedProduct{
			Name:    name,
			Catalog: &store.NewCatalogProduct{DisplayName: name, ShownID: &shown.ID},
		},
	}, nil)
	require.NoError(t, err)
	assert.True(t, result.ProductCreated)
	assert.Nil(t, result.BatchID, "a quantity of 0 creates the product and no batch")

	assert.Equal(t, 1, countRows(t, ctx, `
		SELECT count(*) FROM products p JOIN catalog_products c ON c.id = p.catalog_id
		 WHERE p.id = $1 AND c.base_id = $2`, *result.Item.MatchedProductID, shown.ID))
}

// TestAResolutionThatCannotBeAppliedWritesNothing: every refusal leaves the
// line unresolved and no product, batch or catalog row behind. That includes a
// failure after the product was already created inside the transaction, such
// as another storage's shelf.
func TestAResolutionThatCannotBeAppliedWritesNothing(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newResolveFixture(t, s, "milk")
	foreignShelf, err := s.CreateLocation(ctx, newStorage(t, ctx), store.NewLocation{Name: "Their shelf"})
	require.NoError(t, err)

	newProduct := func(name string) *store.ResolvedProduct {
		return &store.ResolvedProduct{Name: name, Catalog: &store.NewCatalogProduct{DisplayName: name}}
	}

	for label, tc := range map[string]struct {
		in   store.ResolveLine
		want error
	}{
		"both an existing and a new product": {store.ResolveLine{ProductID: &f.milk, NewProduct: newProduct("Oat Milk")}, store.ErrValidation},
		"stock without a product":            {store.ResolveLine{Quantity: 1, LocationID: &f.pantry}, store.ErrValidation},
		"stock without a location":           {store.ResolveLine{ProductID: &f.milk, Quantity: 1}, store.ErrValidation},
		"a negative quantity":                {store.ResolveLine{ProductID: &f.milk, Quantity: -1, LocationID: &f.pantry}, store.ErrValidation},
		"a new product with no name":         {store.ResolveLine{NewProduct: newProduct("  ")}, store.ErrValidation},
		"a new product with no catalog side": {store.ResolveLine{NewProduct: &store.ResolvedProduct{Name: "Oat Milk"}}, store.ErrValidation},
		"another storage's shelf":            {store.ResolveLine{NewProduct: newProduct("Oat Milk " + uuid.NewString()[:8]), Quantity: 1, LocationID: &foreignShelf.ID}, store.ErrNotFound},
		"a catalog row that does not exist":  {store.ResolveLine{NewProduct: &store.ResolvedProduct{Name: "Ghost", AcceptedCatalogID: uuidPtr(uuid.New())}}, store.ErrNotFound},
	} {
		t.Run(label, func(t *testing.T) {
			productsBefore := f.productCount(t)
			catalogBefore := countRows(t, ctx, `SELECT count(*) FROM catalog_products`)

			_, err := s.ResolveShoppingListItem(ctx, f.storageID, f.items[0].ID, tc.in, nil)
			assert.ErrorIs(t, err, tc.want)

			assert.Equal(t, productsBefore, f.productCount(t), "no product is left behind")
			assert.Equal(t, catalogBefore, countRows(t, ctx, `SELECT count(*) FROM catalog_products`), "no catalog row is left behind")
			line, err := s.ShoppingListItemByID(ctx, f.storageID, f.items[0].ID)
			require.NoError(t, err)
			assert.NotEqual(t, store.ItemResolved, line.Status)
		})
	}
	assert.Zero(t, countRows(t, ctx, `
		SELECT count(*) FROM inventory_batches b JOIN products p ON p.id = b.product_id
		 WHERE p.storage_id = $1`, f.storageID), "no batch is left behind")
}
