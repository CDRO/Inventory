package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// catalog_products is the one global table, and the rules on it are abuse
// controls rather than tidiness. It is insert-only because a globally visible
// row that any storage can rewrite is a covert messaging channel between
// households (docs/specs/02-data-model.md).

func TestNormalizeCatalogName(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"Cherry Tomatoes":     "cherry tomatoes",
		"  Cherry Tomatoes  ": "cherry tomatoes",
		"CHERRY   TOMATOES":   "cherry tomatoes",
		"Cherry\tTomatoes":    "cherry tomatoes",
		"cherry\n tomatoes":   "cherry tomatoes",
		"":                    "",
		"   ":                 "",
	}

	for in, want := range tests {
		assert.Equalf(t, want, store.NormalizeCatalogName(in), "normalising %q", in)
	}
}

// TestCatalogIsInsertOnly is the abuse control. A second storage describing
// the same product must not be able to change what the first one wrote —
// that is the covert channel the insert-only rule closes.
func TestCatalogIsInsertOnly(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	name := "Insert Only " + randomSuffix()

	first, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName:  name,
		ItemType:     store.ItemPerishable,
		CategoryPath: ptrString("Food > Dairy"),
	})
	require.NoError(t, err)

	second, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName:  name,
		ItemType:     store.ItemNonPerishable,
		CategoryPath: ptrString("Household > Cleaning"),
		ImageURL:     ptrString("https://example.invalid/hijack.png"),
	})
	require.NoError(t, err, "a conflict is not an error: another storage described it first")

	assert.Equal(t, first.ID, second.ID, "the surviving row is the one written first")
	assert.Equal(t, store.ItemPerishable, second.ItemType, "item_type must not be overwritten")
	require.NotNil(t, second.CategoryPath)
	assert.Equal(t, "Food > Dairy", *second.CategoryPath, "category_path must not be overwritten")
	assert.Nil(t, second.ImageURL, "a later writer must not be able to set the image")

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM catalog_products WHERE normalized_name = $1`,
		store.NormalizeCatalogName(name)))
}

func TestCatalogRejectsBlankName(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{DisplayName: "   "})

	require.ErrorIs(t, err, store.ErrValidation)
}

// TestVariantGraphStaysOneLevel pins the rule that keeps sibling lookup a
// single cheap query. Chains drift semantically: cherry tomato → tomato is
// useful, and a third hop is how "tomato" ends up a variant of "vegetable".
func TestVariantGraphStaysOneLevel(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	suffix := randomSuffix()

	base, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Tomatoes " + suffix,
		ItemType:    store.ItemPerishable,
	})
	require.NoError(t, err)
	assert.Nil(t, base.BaseID, "a first-seen product is its own base")

	cherry, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Cherry Tomatoes " + suffix,
		ItemType:    store.ItemPerishable,
		ShownID:     &base.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, cherry.BaseID)
	assert.Equal(t, base.ID, *cherry.BaseID)

	// The user was shown the *variant* this time. The new row must link to the
	// variant's base, not to the variant — otherwise the graph grows a second
	// level.
	yellow, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Yellow Cherry Tomatoes " + suffix,
		ItemType:    store.ItemPerishable,
		ShownID:     &cherry.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, yellow.BaseID)
	assert.Equal(t, base.ID, *yellow.BaseID,
		"a variant of a variant must link to the shared base, keeping the graph one level deep")

	assert.Equal(t, 0, countRows(t, ctx, `
		SELECT count(*) FROM catalog_products child
		  JOIN catalog_products parent ON child.base_id = parent.id
		 WHERE parent.base_id IS NOT NULL`),
		"no row may point at a row that is itself a variant")
}

func TestVariantSiblingsReturnDisplayNamesOnly(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	suffix := randomSuffix()
	base, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{DisplayName: "Peppers " + suffix})
	require.NoError(t, err)
	_, err = s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Red Peppers " + suffix, ShownID: &base.ID,
	})
	require.NoError(t, err)
	_, err = s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Green Peppers " + suffix, ShownID: &base.ID,
	})
	require.NoError(t, err)

	names, err := s.CatalogVariants(ctx, base.ID, 5)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"Green Peppers " + suffix, "Red Peppers " + suffix}, names)
}

// TestUnknownShownIDDoesNotFailTheInsert — the variant edge is a convenience,
// and the product is still worth recording without it.
func TestUnknownShownIDDoesNotFailTheInsert(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	invented := newUUID(t)

	created, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Orphan " + randomSuffix(),
		ShownID:     &invented,
	})

	require.NoError(t, err)
	assert.Nil(t, created.BaseID)
}

// TestSetCatalogShelfLifeIsTheOnlyPermittedUpdate documents the single
// exception to insert-only. It is a bounded integer written by an admin, so it
// cannot carry a message to another household — unlike the free-text and image
// fields, which stay permanently immutable.
func TestSetCatalogShelfLifeIsTheOnlyPermittedUpdate(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	created, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Hard Cheese " + randomSuffix(),
		ItemType:    store.ItemPerishable,
	})
	require.NoError(t, err)

	require.NoError(t, s.SetCatalogShelfLife(ctx, created.ID, ptrInt(90)))

	after, err := s.FindCatalogProduct(ctx, created.DisplayName)
	require.NoError(t, err)
	require.NotNil(t, after.DefaultShelfLifeDays)
	assert.Equal(t, 90, *after.DefaultShelfLifeDays)

	// Everything else is unchanged.
	assert.Equal(t, created.DisplayName, after.DisplayName)
	assert.Equal(t, created.ItemType, after.ItemType)
}

// TestDeleteCatalogProductLeavesStorageProductsAlone — moderation removes the
// global entry, never a household's own row. products.catalog_id is
// ON DELETE SET NULL precisely so the local product survives.
func TestDeleteCatalogProductLeavesStorageProductsAlone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	entry, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Moderated " + randomSuffix(),
	})
	require.NoError(t, err)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name:      "Local copy",
		CatalogID: &entry.ID,
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteCatalogProduct(ctx, entry.ID))

	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM products WHERE id = $1`, product.ID),
		"deleting a catalog row never touches a storage's own products")

	var catalogID *string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT catalog_id::text FROM products WHERE id = $1`, product.ID).Scan(&catalogID))
	assert.Nil(t, catalogID, "the dangling reference is cleared, not left pointing at nothing")
}

func TestCatalogCarriesNoStorageReference(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	// A structural assertion rather than a behavioural one: the privacy
	// guarantee is that these columns do not exist to be populated by accident.
	forbidden := []string{"storage_id", "user_id", "created_by", "quantity", "location_id"}
	for _, column := range forbidden {
		assert.Equalf(t, 0, countRows(t, ctx, `
			SELECT count(*) FROM information_schema.columns
			 WHERE table_name = 'catalog_products' AND column_name = $1`, column),
			"catalog_products must not have a %s column", column)
	}

	_ = s
}
