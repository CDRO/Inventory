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

func hasAchievement(t *testing.T, ctx context.Context, storageID, userID uuid.UUID, key string) bool {
	t.Helper()
	return countRows(t, ctx, `
		SELECT count(*) FROM achievements_unlocked WHERE storage_id = $1 AND user_id = $2 AND achievement_key = $3`,
		storageID, userID, key) > 0
}

// TestCartographerUnlocksAtThreeLevelsDeep: mapping a location three levels
// deep — a root, a child, and a grandchild — unlocks cartographer for the
// user who mapped the last one, per docs/specs/52-gamification-quests-and-ui.md.
func TestCartographerUnlocksAtThreeLevelsDeep(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	root, err := s.CreateLocationAsUser(ctx, storageID, store.NewLocation{Name: "Basement"}, userID)
	require.NoError(t, err)
	assert.False(t, hasAchievement(t, ctx, storageID, userID, "cartographer"), "one level deep must not unlock it yet")

	child, err := s.CreateLocationAsUser(ctx, storageID, store.NewLocation{Name: "Shelf", ParentID: &root.ID}, userID)
	require.NoError(t, err)
	assert.False(t, hasAchievement(t, ctx, storageID, userID, "cartographer"), "two levels deep must not unlock it yet")

	_, err = s.CreateLocationAsUser(ctx, storageID, store.NewLocation{Name: "Bin", ParentID: &child.ID}, userID)
	require.NoError(t, err)
	assert.True(t, hasAchievement(t, ctx, storageID, userID, "cartographer"), "three levels deep must unlock it")
}

// TestCuratorUnlocksAtTwentyFiveCorrections drives twenty-five real
// AI-correction confirms — the same path
// TestConfirmIngestionRecordsAICorrectionWhenTheReviewerOverrides exercises —
// and checks curator unlocks on the twenty-fifth, not before.
func TestCuratorUnlocksAtTwentyFiveCorrections(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	correctOnce := func(n int) {
		proposed, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Proposed"})
		require.NoError(t, err)
		actual, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Actual"})
		require.NoError(t, err)

		job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
		require.NoError(t, err)
		require.NoError(t, s.CompleteJob(ctx, job.ID, exactMatchProposal(t, "0", proposed.ID)))

		_, err = s.ConfirmIngestion(ctx, storageID, job.ID, &userID, []store.IngestDecision{
			{RowID: "0", Accept: true, ProductID: &actual.ID, Quantity: 1, LocationID: &location.ID},
		})
		require.NoErrorf(t, err, "correction %d", n)
	}

	for i := 1; i < gamification.CuratorCorrections; i++ {
		correctOnce(i)
	}
	assert.False(t, hasAchievement(t, ctx, storageID, userID, "curator"),
		"one correction short of the threshold must not unlock curator")

	correctOnce(gamification.CuratorCorrections)
	assert.True(t, hasAchievement(t, ctx, storageID, userID, "curator"),
		"the 25th correction must unlock curator")
}

func TestStorageAchievementsAwardDeepFreezeToEveryMember(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	bob := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	require.NoError(t, s.AddMember(ctx, storageID, bob))

	for i := 0; i < 100; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item"})
		require.NoError(t, err)
	}

	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))

	assert.True(t, hasAchievement(t, ctx, storageID, alice, "deep_freeze"), "deep_freeze is a household achievement, paid to every member")
	assert.True(t, hasAchievement(t, ctx, storageID, bob, "deep_freeze"))
}

func TestStorageAchievementsDoNotAwardDeepFreezeBelowThreshold(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))

	for i := 0; i < 99; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item"})
		require.NoError(t, err)
	}

	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))

	assert.False(t, hasAchievement(t, ctx, storageID, alice, "deep_freeze"))
}

// TestStorageAchievementsAwardLibrarianAt80PercentHealth builds a storage
// whose health score computes to exactly 80%: categorized, imaged and
// min-stock-tracked are all 100%, recently-active is 100% (one recent
// ledger event), and expiry-tracked is 0% because the one product is
// non_perishable — outside the expiry sub-score's eligible set entirely
// (docs/specs/51-gamification-scoring.md). (100+100+100+0+100)/5 = 80.
func TestStorageAchievementsAwardLibrarianAt80PercentHealth(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))

	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Item", CategoryID: &category.ID, ItemType: store.ItemNonPerishable, MinStock: 1, IconName: ptrString("box"),
	})
	require.NoError(t, err)
	insertLedgerEventAt(t, ctx, product.ID, location.ID, alice, time.Now())

	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))

	assert.True(t, hasAchievement(t, ctx, storageID, alice, "librarian"))
	assert.False(t, hasAchievement(t, ctx, storageID, alice, "archivist"), "80% health must not also cross the 95% archivist bar")
}

// TestStorageAchievementsAwardArchivistAt100PercentHealth adds a tracked
// expiration date on a perishable product's batch on top of librarian's
// fixture, so every sub-score is 100% — comfortably past the 95% archivist
// bar (docs/specs/51-gamification-scoring.md).
func TestStorageAchievementsAwardArchivistAt100PercentHealth(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))

	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Item", CategoryID: &category.ID, ItemType: store.ItemPerishable, MinStock: 1, IconName: ptrString("box"),
	})
	require.NoError(t, err)
	expiry := time.Now().AddDate(0, 0, 14)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		ExpirationDate: &expiry, ExpirationSource: store.ExpirationUser,
		Reason: store.ReasonPurchase, CreatedBy: &alice,
	})
	require.NoError(t, err)

	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))

	assert.True(t, hasAchievement(t, ctx, storageID, alice, "archivist"))
	assert.True(t, hasAchievement(t, ctx, storageID, alice, "librarian"), "100% health must also clear the lower librarian bar")
}

// TestStorageAchievementsSpringCleanBoundary checks the off-by-one the spec's
// own worked example calls out explicitly: "the 101st categorized product in
// a fully-sorted storage is what earns it" — exactly 100 must not unlock it.
func TestStorageAchievementsSpringCleanBoundary(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)

	for i := 0; i < 100; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item", CategoryID: &category.ID})
		require.NoError(t, err)
	}
	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))
	assert.False(t, hasAchievement(t, ctx, storageID, alice, "spring_clean"), "exactly 100 categorized products must not unlock spring_clean yet")

	_, err = s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item", CategoryID: &category.ID})
	require.NoError(t, err)
	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))
	assert.True(t, hasAchievement(t, ctx, storageID, alice, "spring_clean"), "the 101st categorized product must unlock it")
}

func TestStorageAchievementsSpringCleanRequiresZeroUncategorized(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)

	for i := 0; i < 101; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item", CategoryID: &category.ID})
		require.NoError(t, err)
	}
	_, err = s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Uncategorized"})
	require.NoError(t, err)

	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))

	assert.False(t, hasAchievement(t, ctx, storageID, alice, "spring_clean"), "one uncategorized product must block it regardless of count")
}

func TestEvaluateZeroWasteWeekAwardsWhenNothingExpiredUnconsumed(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))

	weekStart := mostRecentMonday(t, time.Now())
	require.NoError(t, s.EvaluateZeroWasteWeek(ctx, storageID, weekStart))

	assert.True(t, hasAchievement(t, ctx, storageID, alice, "zero_waste_week"))
}

func TestEvaluateZeroWasteWeekWithheldWhenSomethingExpiredWithStockRemaining(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Yogurt", ItemType: store.ItemPerishable})
	require.NoError(t, err)

	weekStart := mostRecentMonday(t, time.Now())
	expired := weekStart.AddDate(0, 0, -3) // inside the past week
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 2,
		ExpirationDate: &expired, ExpirationSource: store.ExpirationUser,
		Reason: store.ReasonPurchase, CreatedBy: &alice,
	})
	require.NoError(t, err)

	require.NoError(t, s.EvaluateZeroWasteWeek(ctx, storageID, weekStart))

	assert.False(t, hasAchievement(t, ctx, storageID, alice, "zero_waste_week"),
		"stock that passed its expiry date unconsumed must withhold the achievement")
}

// TestStorageAchievementsAwardWellStockedAfterSevenDays seeds
// well_stocked_since eight days in the past directly, the same way a real
// well-stocked storage would arrive there after several nightly sweeps in a
// row, rather than waiting seven real days in the test.
func TestStorageAchievementsAwardWellStockedAfterSevenDays(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item", MinStock: 1})
	require.NoError(t, err)
	// Stocked at or above its own min_stock, so this product does not itself
	// count as "below" — only an *unstocked* product would reset the streak.
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1,
		ExpirationSource: store.ExpirationDerived, Reason: store.ReasonPurchase, CreatedBy: &alice,
	})
	require.NoError(t, err)

	require.NoError(t, s.UpdateStorageGamificationSettings(ctx, storageID, false, 20))
	_, err = execTest(ctx, `
		UPDATE storage_gamification_settings SET well_stocked_since = $1 WHERE storage_id = $2`,
		time.Now().AddDate(0, 0, -8), storageID)
	require.NoError(t, err)

	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))

	assert.True(t, hasAchievement(t, ctx, storageID, alice, "well_stocked"))
}

func TestStorageAchievementsWellStockedResetsWhenAProductFallsBelowMinStock(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Item", MinStock: 5})
	require.NoError(t, err)

	require.NoError(t, s.EvaluateStorageAchievements(ctx, storageID))

	var since *time.Time
	require.NoError(t, testPool.QueryRow(ctx, `
		SELECT well_stocked_since FROM storage_gamification_settings WHERE storage_id = $1`, storageID).Scan(&since))
	assert.Nil(t, since, "a product below its own min_stock (zero stock against a threshold of 5) must reset well_stocked_since")
}
