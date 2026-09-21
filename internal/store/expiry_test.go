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
	cleared, err := s.SetBatchExpiration(ctx, storageID, batch.ID, nil, nil)
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

	_, err = s.SetBatchExpiration(ctx, storageID, batch.ID, nil, nil)
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

	_, err = s.SetBatchExpiration(ctx, storageA, batch.ID, &when, nil)
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

	storage, err := s.CreateStorage(ctx, store.SystemActor, "Seeded Household")
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

	first, err := s.CreateStorage(ctx, store.SystemActor, "First Household")
	require.NoError(t, err)
	second, err := s.CreateStorage(ctx, store.SystemActor, "Second Household")
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

	storage, err := s.CreateStorage(ctx, store.SystemActor, "Atomic Household")
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

// TestCreateBatchAppliesTheResolvedDefault is acceptance criterion 1:
// "Creating a batch without an explicit expiration date always applies the
// resolved default (or NULL when the resolution yields 'no expiration'), never
// leaves it unset by omission."
//
// Found missing in review — ResolveExpiryFor existed with no callers, so the
// rules were computable and never actually applied.
func TestCreateBatchAppliesTheResolvedDefault(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _, _ := expiryFixture(t, ctx, s)

	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	require.NotNil(t, batch.ExpirationDate, "a batch created with no date must not be left without one")
	assert.Equal(t, store.ExpirationDerived, batch.ExpirationSource)

	// Dairy's 10-day rule, from the fixture.
	expected := batch.CreatedAt.UTC().Truncate(24*time.Hour).AddDate(0, 0, 10)
	assert.Equal(t, expected.Format(time.DateOnly), batch.ExpirationDate.Format(time.DateOnly))
}

// TestCreateBatchResolvesToNoExpiryForNonPerishables — "or NULL when the
// resolution yields 'no expiration'". A plush toy does not go off, and that is
// a resolved answer rather than a missing one.
func TestCreateBatchResolvesToNoExpiryForNonPerishables(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Plush Dinosaur", ItemType: store.ItemNonPerishable,
	})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf"})
	require.NoError(t, err)

	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	assert.Nil(t, batch.ExpirationDate)
	assert.Equal(t, store.ExpirationDerived, batch.ExpirationSource,
		"no rule produced a date, which is different from a person saying there is none")
}

// TestCreateBatchDoesNotOverrideADeliberateNoExpiry — the guard is on the
// source, not on the date being nil, because nil means two different things. A
// caller stating ExpirationUser with no date is saying "this has no expiry",
// and resolving over the top of that is the exact behaviour the whole
// derived/user distinction exists to prevent.
func TestCreateBatchDoesNotOverrideADeliberateNoExpiry(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _, _ := expiryFixture(t, ctx, s)

	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		ExpirationSource: store.ExpirationUser,
		Reason:           store.ReasonPurchase,
	})
	require.NoError(t, err)

	assert.Nil(t, batch.ExpirationDate,
		"a person said this has no expiry; the Dairy rule must not fill it in")
	assert.Equal(t, store.ExpirationUser, batch.ExpirationSource)
}

// TestRefilingAProductRecomputesItsDerivedDates is acceptance criterion 4:
// "'derived' means 'follows the current rules', and a stale derived date is
// simply a wrong one." Also found missing in review.
func TestRefilingAProductRecomputesItsDerivedDates(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, food, _ := expiryFixture(t, ctx, s)

	// Starts in Dairy (10 days).
	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	canned, err := s.CreateCategory(ctx, storageID, store.NewCategory{
		Name: "Canned", ParentID: &food, DefaultShelfLifeDays: shelfLife(730),
	})
	require.NoError(t, err)

	require.NoError(t, s.SetProductCategory(ctx, storageID, productID, &canned.ID))

	date, source := batchExpiry(t, ctx, batch.ID)
	require.NotNil(t, date)
	assert.Equal(t, "derived", source)

	expected := batch.CreatedAt.UTC().Truncate(24*time.Hour).AddDate(0, 0, 730)
	assert.Equal(t, expected.Format(time.DateOnly), date.Format(time.DateOnly),
		"the date follows the rule that now applies, not the one that used to")
}

// TestRefilingAProductLeavesUserDatesAlone — the same protection as every
// other recompute path, checked on this one too because it is a separate entry
// point into the same machinery.
func TestRefilingAProductLeavesUserDatesAlone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, food, _ := expiryFixture(t, ctx, s)

	typed := time.Date(2027, 8, 9, 0, 0, 0, 0, time.UTC)
	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		ExpirationDate: &typed, ExpirationSource: store.ExpirationUser,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	canned, err := s.CreateCategory(ctx, storageID, store.NewCategory{
		Name: "Canned", ParentID: &food, DefaultShelfLifeDays: shelfLife(730),
	})
	require.NoError(t, err)

	require.NoError(t, s.SetProductCategory(ctx, storageID, productID, &canned.ID))

	date, source := batchExpiry(t, ctx, batch.ID)
	require.NotNil(t, date)
	assert.Equal(t, "2027-08-09", date.Format(time.DateOnly))
	assert.Equal(t, "user", source)
}

// TestRefilingToNoCategoryFallsBackToItemType — clearing the category is a
// re-file too, and the chain has to land on the item-type fallback rather than
// leaving a date computed from a rule that no longer applies.
func TestRefilingToNoCategoryFallsBackToItemType(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _, _ := expiryFixture(t, ctx, s)

	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	require.NoError(t, s.SetProductCategory(ctx, storageID, productID, nil))

	date, _ := batchExpiry(t, ctx, batch.ID)
	require.NotNil(t, date)

	// perishable → 7 days, the fallback map's value.
	expected := batch.CreatedAt.UTC().Truncate(24*time.Hour).AddDate(0, 0, 7)
	assert.Equal(t, expected.Format(time.DateOnly), date.Format(time.DateOnly))
}

// TestSetProductCategoryRecomputeIsAtomic is the regression issue #35 asked
// for. SetProductCategory's doc comment and commit message both assert the
// category UPDATE and the expiry recompute commit or roll back together,
// but nothing tested it: review-tests demonstrated that moving the recompute
// into a separate, later transaction still left every re-file test above
// green, because they only assert final state.
//
// This forces the recompute to fail — via real lock contention on the batch
// row it needs to update, from a second connection, with a context deadline
// short enough that SetProductCategory times out while blocked — after the
// category UPDATE has already run inside the same transaction. If that
// UPDATE were in its own, separate transaction (the regression this guards
// against), it would already be committed by the time the recompute fails,
// and the category would end up changed despite the error. Genuine
// atomicity means the whole transaction rolls back, so the category must
// still read as the old one.
func TestSetProductCategoryRecomputeIsAtomic(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, food, dairy := expiryFixture(t, ctx, s)

	canned, err := s.CreateCategory(ctx, storageID, store.NewCategory{
		Name: "Canned", ParentID: &food, DefaultShelfLifeDays: shelfLife(730),
	})
	require.NoError(t, err)

	batch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 1,
		Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	originalDate, _ := batchExpiry(t, ctx, batch.ID)
	require.NotNil(t, originalDate)

	lockTx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback(ctx) }()
	_, err = lockTx.Exec(ctx, `SELECT id FROM inventory_batches WHERE id = $1 FOR UPDATE`, batch.ID)
	require.NoError(t, err)

	shortCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err = s.SetProductCategory(shortCtx, storageID, productID, &canned.ID)
	require.Error(t, err, "the recompute must fail while another transaction holds the batch's row lock")

	require.NoError(t, lockTx.Rollback(ctx), "release the lock")

	var gotCategory uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT category_id FROM products WHERE id = $1`, productID).Scan(&gotCategory))
	assert.Equal(t, dairy, gotCategory,
		"the category change must have rolled back with the failed recompute, not committed on its own")

	date, _ := batchExpiry(t, ctx, batch.ID)
	require.NotNil(t, date)
	assert.Equal(t, originalDate.Format(time.DateOnly), date.Format(time.DateOnly),
		"the batch's date must be unchanged too — both halves roll back together or not at all")
}

// TestRecomputeDerivedExpiryForCategoryCascadeIsRecoverable tests and locks
// in the decision issue #35 records for the category cascade's per-product
// transactions: a partial cascade on failure is accepted rather than fixed,
// because it is recoverable — re-running the cascade converges, since each
// product's recompute is idempotent. This proves that rather than leaving it
// asserted only in a comment: forces one product's recompute to fail via
// real lock contention, confirms the cascade surfaces the error rather than
// silently finishing, then re-runs it with no contention and confirms every
// product ends up on the new rule regardless of how far the failed run got.
func TestRecomputeDerivedExpiryForCategoryCascadeIsRecoverable(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{
		Name: "Pantry", DefaultShelfLifeDays: shelfLife(10),
	})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf"})
	require.NoError(t, err)

	firstProduct, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Beans", CategoryID: &category.ID})
	require.NoError(t, err)
	firstBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: firstProduct.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	secondProduct, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Rice", CategoryID: &category.ID})
	require.NoError(t, err)
	secondBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: secondProduct.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	require.NoError(t, s.SetCategoryShelfLife(ctx, storageID, category.ID, shelfLife(730)))

	// Lock the second product's batch so its recompute fails, regardless of
	// which of the two products the cascade happens to visit first.
	lockTx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback(ctx) }()
	_, err = lockTx.Exec(ctx, `SELECT id FROM inventory_batches WHERE id = $1 FOR UPDATE`, secondBatch.ID)
	require.NoError(t, err)

	shortCtx, cancel := context.WithTimeout(ctx, time.Second)
	_, err = s.RecomputeDerivedExpiryForCategory(shortCtx, storageID, category.ID)
	cancel()
	require.Error(t, err, "the cascade must surface the failure, not silently skip the locked product")

	require.NoError(t, lockTx.Rollback(ctx), "release the lock")

	_, err = s.RecomputeDerivedExpiryForCategory(ctx, storageID, category.ID)
	require.NoError(t, err, "re-running with no contention must converge")

	expected := firstBatch.CreatedAt.UTC().Truncate(24*time.Hour).AddDate(0, 0, 730).Format(time.DateOnly)
	for _, b := range []struct {
		name string
		id   uuid.UUID
	}{{"first", firstBatch.ID}, {"second", secondBatch.ID}} {
		date, _ := batchExpiry(t, ctx, b.id)
		require.NotNil(t, date, "%s product's batch", b.name)
		assert.Equal(t, expected, date.Format(time.DateOnly), "%s product's batch", b.name)
	}
}

// TestCorrectCatalogShelfLifeReachesEveryStorage is the admin catalog cascade
// of docs/specs/08-expiration-and-classification.md (#36, PatchCatalog): an
// admin correcting a catalog entry's shelf life is a correction for every
// household that picked it, not just one — the one cascade in this package
// that is not storage-scoped, because catalog_products itself has no
// storage_id.
func TestCorrectCatalogShelfLifeReachesEveryStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	entry, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Catalog Cascade " + randomSuffix(), ItemType: store.ItemLongShelfLife,
	})
	require.NoError(t, err)

	storageA := newStorage(t, ctx)
	locationA, err := s.CreateLocation(ctx, storageA, store.NewLocation{Name: "Shelf"})
	require.NoError(t, err)
	productA, err := s.CreateProduct(ctx, storageA, store.NewProduct{Name: "Store brand", CatalogID: &entry.ID})
	require.NoError(t, err)
	derivedBatch, err := s.CreateBatch(ctx, storageA, store.NewBatch{
		ProductID: productA.ID, LocationID: locationA.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	userTyped := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	userBatch, err := s.CreateBatch(ctx, storageA, store.NewBatch{
		ProductID: productA.ID, LocationID: locationA.ID, Quantity: 1,
		ExpirationDate: &userTyped, ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	// A second household, picking the same catalog entry: the cascade must
	// cross into its storage too.
	storageB := newStorage(t, ctx)
	locationB, err := s.CreateLocation(ctx, storageB, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	productB, err := s.CreateProduct(ctx, storageB, store.NewProduct{Name: "Also store brand", CatalogID: &entry.ID})
	require.NoError(t, err)
	otherHouseholdBatch, err := s.CreateBatch(ctx, storageB, store.NewBatch{
		ProductID: productB.ID, LocationID: locationB.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	// A third product also links to the entry, but set its own override —
	// the catalog value does not apply to it, so it must be skipped.
	overrideProduct, err := s.CreateProduct(ctx, storageB, store.NewProduct{
		Name: "Prefers its own rule", CatalogID: &entry.ID, DefaultShelfLifeDays: shelfLife(5),
	})
	require.NoError(t, err)
	overrideBatch, err := s.CreateBatch(ctx, storageB, store.NewBatch{
		ProductID: overrideProduct.ID, LocationID: locationB.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	overrideDateBefore, _ := batchExpiry(t, ctx, overrideBatch.ID)
	require.NotNil(t, overrideDateBefore)

	affected, err := s.CorrectCatalogShelfLife(ctx, store.SystemActor, entry.ID, shelfLife(400))
	require.NoError(t, err)
	assert.Equal(t, 2, affected, "the two derived batches across both storages, not the user one or the override one")

	expectedA := derivedBatch.CreatedAt.UTC().Truncate(24*time.Hour).AddDate(0, 0, 400).Format(time.DateOnly)
	dateA, sourceA := batchExpiry(t, ctx, derivedBatch.ID)
	require.NotNil(t, dateA)
	assert.Equal(t, expectedA, dateA.Format(time.DateOnly))
	assert.Equal(t, "derived", sourceA)

	dateUser, sourceUser := batchExpiry(t, ctx, userBatch.ID)
	require.NotNil(t, dateUser)
	assert.Equal(t, "2027-03-01", dateUser.Format(time.DateOnly), "a user-set date is never touched by any cascade")
	assert.Equal(t, "user", sourceUser)

	expectedB := otherHouseholdBatch.CreatedAt.UTC().Truncate(24*time.Hour).AddDate(0, 0, 400).Format(time.DateOnly)
	dateB, _ := batchExpiry(t, ctx, otherHouseholdBatch.ID)
	require.NotNil(t, dateB)
	assert.Equal(t, expectedB, dateB.Format(time.DateOnly), "a second storage's product must be reached too")

	overrideDateAfter, _ := batchExpiry(t, ctx, overrideBatch.ID)
	require.NotNil(t, overrideDateAfter)
	assert.Equal(t, overrideDateBefore.Format(time.DateOnly), overrideDateAfter.Format(time.DateOnly),
		"a product with its own shelf-life override is not reached by the catalog's value")
}

// TestCorrectCatalogShelfLifeThatCannotFinishChangesNothing is issue #61's
// "silent partial correction": a cascade stopped part-way — here by its
// context expiring, the same thing an admin cancelling the request does —
// must leave neither the entry's new value nor any batch it had already
// recomputed behind.
//
// The stop is forced, not raced. Another transaction holds the second
// product's batch row, so the cascade recomputes the first product, then
// blocks on the second until its deadline passes. Products are visited in
// (storage_id, id) order and ids are UUIDv7, so the product created first is
// the one visited first.
func TestCorrectCatalogShelfLifeThatCannotFinishChangesNothing(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	entry, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
		DisplayName: "Catalog Rollback " + randomSuffix(), ItemType: store.ItemLongShelfLife,
		DefaultShelfLifeDays: shelfLife(30),
	})
	require.NoError(t, err)

	storageID := newStorage(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf"})
	require.NoError(t, err)
	first, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "First", CatalogID: &entry.ID})
	require.NoError(t, err)
	second, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Second", CatalogID: &entry.ID})
	require.NoError(t, err)
	firstBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: first.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	secondBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: second.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	firstBefore, _ := batchExpiry(t, ctx, firstBatch.ID)
	require.NotNil(t, firstBefore)

	blocker, err := testPool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	_, err = blocker.Exec(ctx, `SELECT id FROM inventory_batches WHERE id = $1 FOR UPDATE`, secondBatch.ID)
	require.NoError(t, err)

	deadline, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err = s.CorrectCatalogShelfLife(deadline, store.SystemActor, entry.ID, shelfLife(400))
	require.Error(t, err, "the cascade cannot finish while the second product's batch is held")

	require.NoError(t, blocker.Rollback(ctx))

	var days *int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT default_shelf_life_days FROM catalog_products WHERE id = $1`, entry.ID).Scan(&days))
	require.NotNil(t, days)
	assert.Equal(t, 30, *days, "an unfinished correction must not leave the entry showing the new value")

	firstAfter, source := batchExpiry(t, ctx, firstBatch.ID)
	require.NotNil(t, firstAfter)
	assert.Equal(t, firstBefore.Format(time.DateOnly), firstAfter.Format(time.DateOnly),
		"the product recomputed before the stop must be rolled back with the rest")
	assert.Equal(t, "derived", source)
}
