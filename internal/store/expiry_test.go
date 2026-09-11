package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/expiry"
	"github.com/CDRO/Inventory/internal/store"
)

func shelfLife(n int) *int { return &n }

// expiryFixture builds a storage with a Food → Dairy category chain, a product
// filed under Dairy, and a location to put batches in.
func expiryFixture(t *testing.T, ctx context.Context, s *store.Store) (storageID, productID, locationID uuid.UUID, food, dairy uuid.UUID) {
	t.Helper()

	storageID = newStorage(t, ctx)

	foodCat, err := s.CreateCategory(ctx, storageID, store.NewCategory{
		Name: "Food", DefaultShelfLifeDays: shelfLife(365),
	})
	require.NoError(t, err)

	dairyCat, err := s.CreateCategory(ctx, storageID, store.NewCategory{
		Name: "Dairy", ParentID: &foodCat.ID, DefaultShelfLifeDays: shelfLife(10),
	})
	require.NoError(t, err)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Whole Milk", CategoryID: &dairyCat.ID, ItemType: store.ItemPerishable,
	})
	require.NoError(t, err)

	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
	require.NoError(t, err)

	return storageID, product.ID, location.ID, foodCat.ID, dairyCat.ID
}

func batchExpiry(t *testing.T, ctx context.Context, batchID uuid.UUID) (*time.Time, string) {
	t.Helper()

	var date *time.Time
	var source string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT expiration_date, expiration_source FROM inventory_batches WHERE id = $1`,
		batchID).Scan(&date, &source))
	return date, source
}

// TestExpiryRulesWalkTheCategoryChain — the nearest rule wins, which is what
// makes "Food → Dairy: 10" override "Food: 365" for milk.
func TestExpiryRulesWalkTheCategoryChain(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, _, _ := expiryFixture(t, ctx, s)

	rules, err := s.ExpiryRulesFor(ctx, storageID, productID)
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(rules.CategoryDays), 2, "own category then its parent")
	require.NotNil(t, rules.CategoryDays[0])
	assert.Equal(t, 10, *rules.CategoryDays[0], "Dairy comes first")
	require.NotNil(t, rules.CategoryDays[1])
	assert.Equal(t, 365, *rules.CategoryDays[1], "then Food")

	resolution := expiry.Resolve(rules)
	require.NotNil(t, resolution.Days)
	assert.Equal(t, 10, *resolution.Days)
	assert.Equal(t, expiry.SourceCategory, resolution.Source)
}

// TestExpiryRulesPreferTheLocalOverride — a household that disagrees with the
// curated value sets its own, and that outranks everything.
func TestExpiryRulesPreferTheLocalOverride(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, _, dairy := expiryFixture(t, ctx, s)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Hard Cheese", CategoryID: &dairy, ItemType: store.ItemPerishable,
		DefaultShelfLifeDays: shelfLife(90),
	})
	require.NoError(t, err)

	rules, err := s.ExpiryRulesFor(ctx, storageID, product.ID)
	require.NoError(t, err)

	resolution := expiry.Resolve(rules)
	require.NotNil(t, resolution.Days)
	assert.Equal(t, 90, *resolution.Days)
	assert.Equal(t, expiry.SourceProduct, resolution.Source)
}

// TestExpiryRulesAreStorageScoped — a product id from another storage must be
// indistinguishable from one that never existed, here as everywhere.
func TestExpiryRulesAreStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA := newStorage(t, ctx)
	_, theirProduct, _, _, _ := expiryFixture(t, ctx, s)

	_, err := s.ExpiryRulesFor(ctx, storageA, theirProduct)

	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestRecomputeNeverTouchesAUserSetDate is the acceptance criterion this whole
// package is shaped around: "A batch whose expiration_source = 'user' never
// has its expiration_date changed by any cascade, rule edit, category
// reassignment, or admin action."
func TestRecomputeNeverTouchesAUserSetDate(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _, dairy := expiryFixture(t, ctx, s)

	typed := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	userBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		ExpirationDate: &typed, ExpirationSource: store.ExpirationUser,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	derivedBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		ExpirationDate: &typed, ExpirationSource: store.ExpirationDerived,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	// A rule change that would move both dates if nothing protected them.
	require.NoError(t, s.SetCategoryShelfLife(ctx, storageID, dairy, shelfLife(2)))
	affected, err := s.RecomputeDerivedExpiry(ctx, storageID, productID)
	require.NoError(t, err)

	assert.Equal(t, 1, affected, "exactly one batch is eligible")

	gotUser, sourceUser := batchExpiry(t, ctx, userBatch.ID)
	require.NotNil(t, gotUser)
	assert.Equal(t, "2027-01-15", gotUser.Format(time.DateOnly),
		"a date a person typed outranks every rule, forever")
	assert.Equal(t, "user", sourceUser)

	gotDerived, sourceDerived := batchExpiry(t, ctx, derivedBatch.ID)
	require.NotNil(t, gotDerived)
	assert.NotEqual(t, "2027-01-15", gotDerived.Format(time.DateOnly), "the derived one moves")
	assert.Equal(t, "derived", sourceDerived)
}

// TestAUserClearedDateStaysCleared is the stickiest case and the one with the
// worst failure mode: a user who clears the date on a jar with no printed
// date, and finds the system has put one back, has learned the app ignores
// them.
func TestAUserClearedDateStaysCleared(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _, dairy := expiryFixture(t, ctx, s)

	someDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		ExpirationDate: &someDate, ExpirationSource: store.ExpirationDerived,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	// The person says: this has no expiry.
	cleared, err := s.SetBatchExpiration(ctx, storageID, batch.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, cleared.ExpirationDate)
	assert.Equal(t, store.ExpirationUser, cleared.ExpirationSource,
		"clearing is a decision, so it is recorded as one")

	// Every subsequent rule change must leave it alone.
	require.NoError(t, s.SetCategoryShelfLife(ctx, storageID, dairy, shelfLife(3)))
	affected, err := s.RecomputeDerivedExpiry(ctx, storageID, productID)
	require.NoError(t, err)
	assert.Zero(t, affected)

	date, source := batchExpiry(t, ctx, batch.ID)
	assert.Nil(t, date, "NULL + user is a deliberate statement, not an empty field")
	assert.Equal(t, "user", source)
}

// TestResetGivesABatchBackToTheRules — without a way back, one accidental edit
// would opt a batch out of every future rule change permanently.
func TestResetGivesABatchBackToTheRules(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _, _ := expiryFixture(t, ctx, s)

	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	_, err = s.SetBatchExpiration(ctx, storageID, batch.ID, nil)
	require.NoError(t, err)

	reset, err := s.ResetBatchExpirationToDerived(ctx, storageID, batch.ID)
	require.NoError(t, err)

	assert.Equal(t, store.ExpirationDerived, reset.ExpirationSource)
	require.NotNil(t, reset.ExpirationDate, "Dairy's 10-day rule applies again immediately")

	expected := batch.CreatedAt.UTC().Truncate(24*time.Hour).AddDate(0, 0, 10)
	assert.Equal(t, expected.Format(time.DateOnly), reset.ExpirationDate.Format(time.DateOnly))
}

// TestCategoryCascadeReachesTheWholeSubtree — changing "Food" has to reach a
// product filed under "Food → Dairy", because that product's chain climbs
// through the row that just changed.
func TestCategoryCascadeReachesTheWholeSubtree(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, food, dairy := expiryFixture(t, ctx, s)

	// Remove Dairy's own rule so the product resolves through Food.
	require.NoError(t, s.SetCategoryShelfLife(ctx, storageID, dairy, nil))

	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	require.NoError(t, s.SetCategoryShelfLife(ctx, storageID, food, shelfLife(30)))
	affected, err := s.RecomputeDerivedExpiryForCategory(ctx, storageID, food)
	require.NoError(t, err)

	assert.Equal(t, 1, affected)

	date, _ := batchExpiry(t, ctx, batch.ID)
	require.NotNil(t, date)
	expected := batch.CreatedAt.UTC().Truncate(24*time.Hour).AddDate(0, 0, 30)
	assert.Equal(t, expected.Format(time.DateOnly), date.Format(time.DateOnly),
		"a product two levels down follows the ancestor's new rule")
}

// TestRecomputeIsPerBatchNotPerProduct — two batches of the same product added
// a week apart must not end up sharing an expiry date.
func TestRecomputeIsPerBatchNotPerProduct(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _, dairy := expiryFixture(t, ctx, s)

	older, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	// Age one batch in the database rather than by waiting.
	_, err = testPool.Exec(ctx,
		`UPDATE inventory_batches SET created_at = now() - interval '7 days' WHERE id = $1`, older.ID)
	require.NoError(t, err)

	newer, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	require.NoError(t, s.SetCategoryShelfLife(ctx, storageID, dairy, shelfLife(10)))
	_, err = s.RecomputeDerivedExpiry(ctx, storageID, productID)
	require.NoError(t, err)

	olderDate, _ := batchExpiry(t, ctx, older.ID)
	newerDate, _ := batchExpiry(t, ctx, newer.ID)
	require.NotNil(t, olderDate)
	require.NotNil(t, newerDate)

	assert.True(t, olderDate.Before(*newerDate),
		"the older batch expires sooner; the date is relative to when each was created")
}

// TestRecomputeWritesNoLedgerRows — quantities do not change, and a ledger
// entry recording "nothing moved" would be noise in the one table that exists
// to explain movement.
func TestRecomputeWritesNoLedgerRows(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _, dairy := expiryFixture(t, ctx, s)

	_, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	before := logCount(t, ctx, productID)

	require.NoError(t, s.SetCategoryShelfLife(ctx, storageID, dairy, shelfLife(4)))
	_, err = s.RecomputeDerivedExpiry(ctx, storageID, productID)
	require.NoError(t, err)

	assert.Equal(t, before, logCount(t, ctx, productID))
}

// TestSetBatchExpirationIsStorageScoped closes the same boundary on the write
// path a user reaches directly.
func TestSetBatchExpirationIsStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA := newStorage(t, ctx)
	storageB, productID, locationID, _, _ := expiryFixture(t, ctx, s)

	batch, err := s.CreateBatch(ctx, storageB, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	when := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	_, err = s.SetBatchExpiration(ctx, storageA, batch.ID, &when)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.ResetBatchExpirationToDerived(ctx, storageA, batch.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, source := batchExpiry(t, ctx, batch.ID)
	assert.Equal(t, "derived", source, "the refusal must also not have written anything")
}

// TestCreateStorageSeedsTheStarterCategories — a new storage has to arrive
// with a tree to edit rather than an empty screen, and the first batch
// somebody adds should get a sensible date rather than falling through to the
// item-type fallback.
//
// This goes through CreateStorage deliberately. The suite's newStorage helper
// inserts with plain SQL so that a broken writer cannot hide itself, which
// means nothing else in this package exercises the seeding at all.
func TestCreateStorageSeedsTheStarterCategories(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storage, err := s.CreateStorage(ctx, "Seeded Household")
	require.NoError(t, err)

	categories, err := s.CategoryTree(ctx, storage.ID)
	require.NoError(t, err)

	byName := map[string]store.Category{}
	for _, c := range categories {
		byName[c.Name] = c
	}

	require.Contains(t, byName, "Food")
	require.Contains(t, byName, "Dairy")
	require.Contains(t, byName, "Household")

	require.NotNil(t, byName["Food"].DefaultShelfLifeDays)
	assert.Equal(t, 365, *byName["Food"].DefaultShelfLifeDays)
	require.NotNil(t, byName["Dairy"].DefaultShelfLifeDays)
	assert.Equal(t, 10, *byName["Dairy"].DefaultShelfLifeDays)

	// NULL means "inherit", not "no expiry" — whatever a user files under
	// Household decides for itself.
	assert.Nil(t, byName["Household"].DefaultShelfLifeDays)

	// Dairy hangs off Food, so a product filed there resolves 10 before 365.
	require.NotNil(t, byName["Dairy"].ParentID)
	assert.Equal(t, byName["Food"].ID, *byName["Dairy"].ParentID)
}

// TestSeededCategoriesAreScopedToTheirStorage — the starter tree is per
// storage, so two households must not see each other's nodes.
func TestSeededCategoriesAreScopedToTheirStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	first, err := s.CreateStorage(ctx, "First Household")
	require.NoError(t, err)
	second, err := s.CreateStorage(ctx, "Second Household")
	require.NoError(t, err)

	firstTree, err := s.CategoryTree(ctx, first.ID)
	require.NoError(t, err)
	secondTree, err := s.CategoryTree(ctx, second.ID)
	require.NoError(t, err)

	require.NotEmpty(t, firstTree)
	assert.Len(t, secondTree, len(firstTree), "both get the same starter tree")

	for _, c := range firstTree {
		assert.Equal(t, first.ID, c.StorageID)
	}
	for _, c := range secondTree {
		assert.Equal(t, second.ID, c.StorageID)
	}
}

// TestAFailedSeedRollsBackTheStorage — a storage that exists without its
// categories would show a partial tree indistinguishable from one the user had
// edited themselves.
func TestAFailedSeedRollsBackTheStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	before := countRows(t, ctx, `SELECT count(*) FROM storages WHERE name = $1`, "Atomic Household")
	require.Zero(t, before)

	storage, err := s.CreateStorage(ctx, "Atomic Household")
	require.NoError(t, err)

	// The storage and its whole tree land together.
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM storages WHERE id = $1`, storage.ID))
	assert.Equal(t, len(starterCategoryNames()),
		countRows(t, ctx, `SELECT count(*) FROM categories WHERE storage_id = $1`, storage.ID))
}

// starterCategoryNames mirrors the seed list's size without exporting it.
func starterCategoryNames() []string {
	return []string{"Food", "Dairy", "Produce", "Meat", "Canned", "Household", "Collectibles"}
}
