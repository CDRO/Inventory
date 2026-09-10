package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
)

// TestSimilarProductsNeverCrossesAStorageBoundary is the privacy invariant at
// its most tempting failure point: trigram search is the one query in the
// system whose natural form ("find things named like this") has no reason to
// mention a storage at all, so a forgotten WHERE clause here would not look
// wrong and would leak the names of another household's products.
func TestSimilarProductsNeverCrossesAStorageBoundary(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)

	_, err := s.CreateProduct(ctx, storageB, store.NewProduct{Name: "Cherry Tomatoes"})
	require.NoError(t, err)

	found, err := s.SimilarProducts(ctx, storageA, "cherry tomatoes", matching.AmbiguousThreshold, 3)
	require.NoError(t, err)

	assert.Empty(t, found, "an identically named product in another storage must be invisible")

	// And the same query in the storage that owns it does find it, so the
	// emptiness above is scoping rather than a broken query.
	found, err = s.SimilarProducts(ctx, storageB, "cherry tomatoes", matching.AmbiguousThreshold, 3)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "Cherry Tomatoes", found[0].Name)
}

func TestSimilarProductsRanksAndBounds(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	for _, name := range []string{"Whole Milk", "Oat Milk", "Milk Chocolate", "Bicycle Pump"} {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: name})
		require.NoError(t, err)
	}

	found, err := s.SimilarProducts(ctx, storageID, "milk", matching.AmbiguousThreshold, 3)
	require.NoError(t, err)

	require.NotEmpty(t, found)
	assert.LessOrEqual(t, len(found), 3, "the limit is what keeps an ambiguous prompt answerable")

	for i := 1; i < len(found); i++ {
		assert.GreaterOrEqual(t, found[i-1].Similarity, found[i].Similarity,
			"results must arrive ranked, since the caller reads [0] as the best")
	}
	for _, c := range found {
		assert.GreaterOrEqual(t, c.Similarity, matching.AmbiguousThreshold)
		assert.NotEqual(t, "Bicycle Pump", c.Name, "nothing below the floor may appear")
	}
}

// TestSimilarCatalogProductIsNotStorageScoped is the deliberate opposite of the
// test above, and the reason the catalog is safe to share: the table has no
// storage column, so a lookup cannot be scoped and cannot reveal ownership
// either. Both storages see the same description because it describes a
// product, not a pantry.
func TestSimilarCatalogProductIsNotStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	shelfLife := 14
	category := "Food > Produce"
	_, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName:          "Beefsteak Tomatoes",
		CategoryPath:         &category,
		ItemType:             store.ItemPerishable,
		DefaultShelfLifeDays: &shelfLife,
	})
	require.NoError(t, err)

	hit, err := s.SimilarCatalogProduct(ctx, "beefsteak tomatoes", matching.AmbiguousThreshold)
	require.NoError(t, err)
	require.NotNil(t, hit)

	assert.Equal(t, "Beefsteak Tomatoes", hit.DisplayName)
	require.NotNil(t, hit.CategoryPath)
	assert.Equal(t, "Food > Produce", *hit.CategoryPath)
	require.NotNil(t, hit.DefaultShelfLifeDays)
	assert.Equal(t, 14, *hit.DefaultShelfLifeDays,
		"pre-filled shelf life is what lets a catalog hit be accepted in one click")
}

func TestSimilarCatalogProductMissesBelowTheFloor(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Sourdough Starter", ItemType: store.ItemPerishable,
	})
	require.NoError(t, err)

	hit, err := s.SimilarCatalogProduct(ctx, "bicycle inner tube", matching.AmbiguousThreshold)
	require.NoError(t, err)

	assert.Nil(t, hit, "a miss is nil and not an error — it is the ordinary case")
}

// TestCatalogVariantsOf covers the lookup that makes an abbreviated list line
// usable: "thomatoes, c." trigram-matches the base, and the variants are what
// turn that into a one-click "cherry tomatoes".
func TestCatalogVariantsOf(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	base, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Tomatoes", ItemType: store.ItemPerishable,
	})
	require.NoError(t, err)

	cherry, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Cherry Tomatoes", ItemType: store.ItemPerishable, ShownID: &base.ID,
	})
	require.NoError(t, err)

	_, err = s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Yellow Tomatoes", ItemType: store.ItemPerishable, ShownID: &base.ID,
	})
	require.NoError(t, err)

	t.Run("a base offers its children", func(t *testing.T) {
		variants, err := s.CatalogVariantsOf(ctx, base.ID, "cherry", matching.MaxVariants)
		require.NoError(t, err)

		names := variantNames(variants)
		assert.Contains(t, names, "Cherry Tomatoes")
		assert.Contains(t, names, "Yellow Tomatoes")
		assert.NotContains(t, names, "Tomatoes", "a node is never its own alternative")
		assert.Equal(t, "Cherry Tomatoes", variants[0].DisplayName,
			"ranked by similarity to what the user actually typed")
	})

	t.Run("a variant offers its siblings", func(t *testing.T) {
		variants, err := s.CatalogVariantsOf(ctx, cherry.ID, "tomatoes", matching.MaxVariants)
		require.NoError(t, err)

		names := variantNames(variants)
		assert.Contains(t, names, "Yellow Tomatoes")
		assert.NotContains(t, names, "Cherry Tomatoes")
	})

	t.Run("the limit is respected", func(t *testing.T) {
		variants, err := s.CatalogVariantsOf(ctx, base.ID, "tomatoes", 1)
		require.NoError(t, err)
		assert.Len(t, variants, 1)
	})
}

func variantNames(variants []matching.CatalogVariant) []string {
	out := make([]string, 0, len(variants))
	for _, v := range variants {
		out = append(out, v.DisplayName)
	}
	return out
}
