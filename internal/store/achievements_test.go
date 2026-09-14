package store_test

import (
	"context"
	"testing"

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
