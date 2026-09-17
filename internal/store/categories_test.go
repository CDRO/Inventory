package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/gamification"
	"github.com/CDRO/Inventory/internal/store"
)

// The write half of the category tree (issue #72): UpdateCategory backs
// PATCH .../categories/{id}, and CreateCategoryAsUser backs the POST. The
// same-storage and cycle rules are shared with locations through tree.go and
// covered there; these tests cover what is particular to categories — the
// expiry dates that hang off the tree's shape, and the contribution a new node
// earns.

func TestUpdateCategoryRenameLeavesTheParentAlone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	food, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	dairy, err := s.CreateCategory(ctx, storageID, store.NewCategory{
		Name: "Dairy", ParentID: &food.ID, DefaultShelfLifeDays: shelfLife(10),
	})
	require.NoError(t, err)

	renamed := "Dairy & Eggs"
	updated, err := s.UpdateCategory(ctx, storageID, dairy.ID, store.CategoryPatch{Name: &renamed})
	require.NoError(t, err)

	assert.Equal(t, "Dairy & Eggs", updated.Name)
	require.NotNil(t, updated.ParentID, "a rename must not detach the node from its parent")
	assert.Equal(t, food.ID, *updated.ParentID)
	require.NotNil(t, updated.DefaultShelfLifeDays, "nor erase a shelf-life rule nobody mentioned")
	assert.Equal(t, 10, *updated.DefaultShelfLifeDays)
	assert.True(t, updated.UpdatedAt.After(dairy.UpdatedAt), "a rename is a change a client delta must see")
}

func TestUpdateCategoryMovesAndRenamesInOneCall(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	food, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	household, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Household"})
	require.NoError(t, err)
	soap, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Soap", ParentID: &food.ID})
	require.NoError(t, err)

	t.Run("rename and re-parent together", func(t *testing.T) {
		renamed := "Dish soap"
		updated, err := s.UpdateCategory(ctx, storageID, soap.ID, store.CategoryPatch{
			Name: &renamed, ParentID: &household.ID, SetParentID: true,
		})
		require.NoError(t, err)
		assert.Equal(t, "Dish soap", updated.Name)
		require.NotNil(t, updated.ParentID)
		assert.Equal(t, household.ID, *updated.ParentID)
	})

	t.Run("an explicit null parent promotes the node to a root", func(t *testing.T) {
		updated, err := s.UpdateCategory(ctx, storageID, soap.ID, store.CategoryPatch{SetParentID: true})
		require.NoError(t, err)
		assert.Nil(t, updated.ParentID)
		assert.Equal(t, "Dish soap", updated.Name, "a move alone keeps the name")
	})
}

func TestUpdateCategoryRejectsCyclesAndForeignEnds(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, otherStorage := twoStorages(t, ctx)

	food, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	dairy, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy", ParentID: &food.ID})
	require.NoError(t, err)
	theirs, err := s.CreateCategory(ctx, otherStorage, store.NewCategory{Name: "Their Food"})
	require.NoError(t, err)

	_, err = s.UpdateCategory(ctx, storageID, food.ID, store.CategoryPatch{ParentID: &dairy.ID, SetParentID: true})
	assert.ErrorIs(t, err, store.ErrConflict, "a node cannot move under its own child")

	_, err = s.UpdateCategory(ctx, storageID, food.ID, store.CategoryPatch{ParentID: &theirs.ID, SetParentID: true})
	assert.ErrorIs(t, err, store.ErrNotFound, "a parent in another storage is not found")

	renamed := "Mine now"
	_, err = s.UpdateCategory(ctx, storageID, theirs.ID, store.CategoryPatch{Name: &renamed})
	assert.ErrorIs(t, err, store.ErrNotFound, "a node in another storage is not found")

	_, err = s.UpdateCategory(ctx, storageID, newUUID(t), store.CategoryPatch{Name: &renamed})
	assert.ErrorIs(t, err, store.ErrNotFound, "and neither is one that does not exist")

	var name string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT name FROM categories WHERE id = $1`, theirs.ID).Scan(&name))
	assert.Equal(t, "Their Food", name)
}

// TestMovingACategoryRecomputesItsSubtreesDerivedDates: a product filed under
// Food → Dairy → Cheese that inherits Food's rule follows Dairy when Dairy is
// moved under Canned, because its chain now climbs through Canned instead.
// The product sits two levels below the node that moved, so this also proves
// the recompute walks the whole subtree rather than the moved node's direct
// products only.
func TestMovingACategoryRecomputesItsSubtreesDerivedDates(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	food, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food", DefaultShelfLifeDays: shelfLife(365)})
	require.NoError(t, err)
	canned, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Canned", DefaultShelfLifeDays: shelfLife(730)})
	require.NoError(t, err)
	dairy, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Dairy", ParentID: &food.ID})
	require.NoError(t, err)
	cheese, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Cheese", ParentID: &dairy.ID})
	require.NoError(t, err)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Aged Gouda", CategoryID: &cheese.ID, ItemType: store.ItemPerishable,
	})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Cellar"})
	require.NoError(t, err)

	derived, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	typed := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	userSet, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		ExpirationDate: &typed, ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	day := derived.CreatedAt.UTC().Truncate(24 * time.Hour)
	before, _ := batchExpiry(t, ctx, derived.ID)
	require.NotNil(t, before)
	require.Equal(t, day.AddDate(0, 0, 365).Format(time.DateOnly), before.Format(time.DateOnly),
		"precondition: the batch starts on Food's rule")

	_, err = s.UpdateCategory(ctx, storageID, dairy.ID, store.CategoryPatch{ParentID: &canned.ID, SetParentID: true})
	require.NoError(t, err)

	after, source := batchExpiry(t, ctx, derived.ID)
	require.NotNil(t, after)
	assert.Equal(t, day.AddDate(0, 0, 730).Format(time.DateOnly), after.Format(time.DateOnly),
		"a derived date follows the rule the moved subtree now inherits")
	assert.Equal(t, "derived", source)

	kept, keptSource := batchExpiry(t, ctx, userSet.ID)
	require.NotNil(t, kept)
	assert.Equal(t, "2027-06-01", kept.Format(time.DateOnly), "a date a person set is never touched by a move")
	assert.Equal(t, "user", keptSource)
}

// TestCreateCategoryAsUserRecordsCategoryCreated is the category counterpart
// of TestCreateLocationAsUserRecordsLocationMapped.
func TestCreateCategoryAsUserRecordsCategoryCreated(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	created, err := s.CreateCategoryAsUser(ctx, storageID, store.NewCategory{
		Name: "Spices", DefaultShelfLifeDays: shelfLife(1095),
	}, userID)
	require.NoError(t, err)
	require.NotNil(t, created.DefaultShelfLifeDays)
	assert.Equal(t, 1095, *created.DefaultShelfLifeDays)

	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindCategoryCreated))
	var ref uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT ref_id FROM contribution_events WHERE storage_id = $1 AND kind = 'category_created'`, storageID).Scan(&ref))
	assert.Equal(t, created.ID, ref)

	xp, _, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, gamification.XPCategoryCreated, xp)

	// A refused create earns nothing: the contribution shares the insert's
	// transaction.
	_, err = s.CreateCategoryAsUser(ctx, storageID, store.NewCategory{Name: "Orphan", ParentID: ptrUUID(newUUID(t))}, userID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindCategoryCreated))
}

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }
