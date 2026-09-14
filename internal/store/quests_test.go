package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// countQuests is a small assertion helper for this file's tests.
func countQuests(t *testing.T, ctx context.Context, storageID uuid.UUID) int {
	t.Helper()
	return countRows(t, ctx, `SELECT count(*) FROM quests WHERE storage_id = $1`, storageID)
}

// newAllCleanStorage builds a storage with ten fully-specified products and
// no gaps at all — every generator in
// docs/specs/52-gamification-quests-and-ui.md's table should decline to
// fire, so generation should produce zero quests. A member is put on
// holiday for the current week specifically to suppress consumption_hygiene
// without needing a real consumption log against a batch.
func newAllCleanStorage(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()
	s := requireDB(t)

	storageID := newStorage(t, ctx)
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)

	for i := 0; i < 10; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name:       "Product",
			CategoryID: &category.ID,
			ItemType:   store.ItemNonPerishable,
			MinStock:   1,
			IconName:   ptrString("box"),
		})
		require.NoError(t, err)
	}

	member := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, member))
	insertHolidayWeek(t, ctx, member, mostRecentMonday(t, time.Now()))

	return storageID
}

func TestGenerateWeeklyQuestsAllClearProducesNoQuests(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newAllCleanStorage(t, ctx)

	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, time.Now()))

	assert.Equal(t, 0, countQuests(t, ctx, storageID), "a storage with no gaps must generate zero quests, not filler")

	var cleanSince *time.Time
	err := testPool.QueryRow(ctx,
		`SELECT clean_since FROM storage_gamification_settings WHERE storage_id = $1`, storageID).Scan(&cleanSince)
	require.NoError(t, err)
	require.NotNil(t, cleanSince, "an all-clear generation must start the clean streak")
}

func TestGenerateWeeklyQuestsFiresUncategorizedAtFixedTargetOfFive(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	// Six uncategorized products: the generator fires at >= 5, but the
	// rendered quest text ("Sort 5 products into categories") and the stored
	// target are always exactly 5, not however many actually qualify.
	for i := 0; i < 6; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Mystery item"})
		require.NoError(t, err)
	}

	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, time.Now()))

	var generator string
	var targetCount, xpReward int
	err := testPool.QueryRow(ctx, `
		SELECT generator, target_count, xp_reward FROM quests WHERE storage_id = $1 AND generator = 'uncategorized'`,
		storageID).Scan(&generator, &targetCount, &xpReward)
	require.NoError(t, err, "the uncategorized generator must have fired")
	assert.Equal(t, 5, targetCount)
	assert.Equal(t, 20, xpReward)
}

func TestGenerateWeeklyQuestsNeverPadsBelowThree(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	// Only one generator's condition holds (five uncategorized products);
	// everything else about this fresh storage is otherwise clean enough that
	// it must not be padded up to three quests.
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)
	// Five products missing only a category — otherwise fully specified, so
	// imageless and untracked_reorder do not also fire off the same rows.
	for i := 0; i < 5; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name: "Mystery item", ItemType: store.ItemNonPerishable, MinStock: 1, IconName: ptrString("box"),
		})
		require.NoError(t, err)
	}
	// Ten more, fully specified including category, so first_mile does not
	// also fire.
	for i := 0; i < 10; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name: "Fine item", CategoryID: &category.ID, ItemType: store.ItemNonPerishable, MinStock: 1, IconName: ptrString("box"),
		})
		require.NoError(t, err)
	}
	member := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, member))
	insertHolidayWeek(t, ctx, member, mostRecentMonday(t, time.Now()))

	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, time.Now()))

	assert.Equal(t, 1, countQuests(t, ctx, storageID), "only one generator fired; the week must not be padded to three")
}

func TestGenerateWeeklyQuestsIsIdempotentPerWeek(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	for i := 0; i < 6; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Mystery item"})
		require.NoError(t, err)
	}

	now := time.Now()
	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, now))
	firstRunCount := countQuests(t, ctx, storageID)
	require.Greater(t, firstRunCount, 0)

	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, now))

	assert.Equal(t, firstRunCount, countQuests(t, ctx, storageID),
		"re-running generation for the same week must not duplicate quests, via the UNIQUE(storage_id, week_start, generator) constraint")
}

// TestAdvanceQuestsCompletesAndPaysEveryContributor is the full pipeline:
// generate a quest, have two different users each close part of its target,
// and confirm both — not just whoever finished it — receive the full reward
// once (docs/specs/52-gamification-quests-and-ui.md's quest XP attribution
// rule).
func TestAdvanceQuestsCompletesAndPaysEveryContributor(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)

	products := make([]uuid.UUID, 5)
	for i := range products {
		// Missing only a category, so uncategorized is the sole generator
		// that fires — otherwise the XP totals asserted below would also
		// include an imageless or untracked_reorder quest reward.
		p, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name: "Mystery item", ItemType: store.ItemNonPerishable, MinStock: 1, IconName: ptrString("box"),
		})
		require.NoError(t, err)
		products[i] = p.ID
	}
	// Five more, fully specified including category, so first_mile does not
	// also fire (the storage would otherwise have fewer than ten products).
	for i := 0; i < 5; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name: "Fine item", CategoryID: &category.ID, ItemType: store.ItemNonPerishable, MinStock: 1, IconName: ptrString("box"),
		})
		require.NoError(t, err)
	}

	// Suppresses consumption_hygiene, which would otherwise also fire: this
	// storage has never logged a consumption.
	holidayMember := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, holidayMember))
	insertHolidayWeek(t, ctx, holidayMember, mostRecentMonday(t, time.Now()))

	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, time.Now()))
	require.Equal(t, 1, countQuests(t, ctx, storageID))

	alice := newUser(t, ctx)
	bob := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	require.NoError(t, s.AddMember(ctx, storageID, bob))

	// Alice closes four of the five; the quest must not complete yet.
	for _, p := range products[:4] {
		require.NoError(t, s.SetProductCategoryAsUser(ctx, storageID, p, &category.ID, alice))
	}
	var completedAt *time.Time
	require.NoError(t, testPool.QueryRow(ctx, `SELECT completed_at FROM quests WHERE storage_id = $1`, storageID).Scan(&completedAt))
	assert.Nil(t, completedAt, "four of five is not enough to complete a target-5 quest")

	aliceProgress, err := s.UserProgressInStorage(ctx, storageID, alice)
	require.NoError(t, err)
	assert.Equal(t, 4*2, aliceProgress.XP, "base metadata_filled XP accrues per contribution, but no quest reward before completion")

	// Bob closes the fifth: the quest completes now.
	require.NoError(t, s.SetProductCategoryAsUser(ctx, storageID, products[4], &category.ID, bob))

	require.NoError(t, testPool.QueryRow(ctx, `SELECT completed_at FROM quests WHERE storage_id = $1`, storageID).Scan(&completedAt))
	require.NotNil(t, completedAt, "the fifth categorization must complete the quest")

	aliceProgress, err = s.UserProgressInStorage(ctx, storageID, alice)
	require.NoError(t, err)
	bobProgress, err := s.UserProgressInStorage(ctx, storageID, bob)
	require.NoError(t, err)

	// Both get the full 20 XP quest reward on top of their 2 XP
	// metadata_filled contributions — not split, and not finisher-only.
	assert.Equal(t, 20+4*2, aliceProgress.XP, "alice contributed 4 qualifying actions and must get the full reward, not a split share")
	assert.Equal(t, 20+2, bobProgress.XP, "bob finished the quest but must not be paid more than the full reward")

	contributorCount := countRows(t, ctx, `
		SELECT count(*) FROM quest_contributors qc JOIN quests q ON q.id = qc.quest_id WHERE q.storage_id = $1`, storageID)
	assert.Equal(t, 2, contributorCount, "exactly the two contributors, once each")
}

// TestAdvanceQuestsIgnoresATargetNotInTheQuestsCandidateSet is the
// regression for #54 finding 2: quest-contributor crediting used to match
// on write kind alone, so filling in a category on any product — even one
// that did not exist yet when the quest generated, and so was never in its
// frozen candidate set — would still credit the actor as a contributor.
func TestAdvanceQuestsIgnoresATargetNotInTheQuestsCandidateSet(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)

	// Five uncategorized products so the quest generates and freezes exactly
	// these five ids into its params.
	for i := 0; i < 5; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name: "Mystery item", ItemType: store.ItemNonPerishable, MinStock: 1, IconName: ptrString("box"),
		})
		require.NoError(t, err)
	}
	// Five more, fully specified including category, so first_mile does not
	// also fire (the storage would otherwise have fewer than ten products).
	for i := 0; i < 5; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name: "Fine item", CategoryID: &category.ID, ItemType: store.ItemNonPerishable, MinStock: 1, IconName: ptrString("box"),
		})
		require.NoError(t, err)
	}

	holidayMember := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, holidayMember))
	insertHolidayWeek(t, ctx, holidayMember, mostRecentMonday(t, time.Now()))

	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, time.Now()))
	require.Equal(t, 1, countQuests(t, ctx, storageID))

	// Created after generation, so it is not in the frozen candidate set —
	// even though it is uncategorized the same way the five above are.
	late, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Late item", ItemType: store.ItemNonPerishable, MinStock: 1, IconName: ptrString("box"),
	})
	require.NoError(t, err)

	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	require.NoError(t, s.SetProductCategoryAsUser(ctx, storageID, late.ID, &category.ID, alice))

	contributorCount := countRows(t, ctx, `
		SELECT count(*) FROM quest_contributors qc JOIN quests q ON q.id = qc.quest_id WHERE q.storage_id = $1`, storageID)
	assert.Equal(t, 0, contributorCount, "a product outside the quest's frozen candidate set must not credit a contributor")
}

// TestAdvanceQuestsCreditsOnlyTheGeneratorTheFieldActuallyAdvanced is the
// regression review-go asked for on #54 finding 2's first fix: a product
// with no category, no image, and no min_stock qualifies for all three of
// uncategorized, imageless and untracked_reorder at once, and every one of
// those three fills records the identical metadata_filled kind with the
// identical product-id refID. Narrowing quest-contributor credit by id
// alone (questTargetQualifies) is not enough to tell them apart: filling in
// only the category must not also credit imageless or untracked_reorder,
// even though the product id qualifies for both of those quests' frozen
// candidate sets too.
func TestAdvanceQuestsCreditsOnlyTheGeneratorTheFieldActuallyAdvanced(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Staples"})
	require.NoError(t, err)

	// Five products with no category, no image, and no min_stock — each
	// qualifies for uncategorized, imageless, and untracked_reorder at once.
	var blank []uuid.UUID
	for i := 0; i < 5; i++ {
		p, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Blank item", ItemType: store.ItemNonPerishable})
		require.NoError(t, err)
		blank = append(blank, p.ID)
	}
	// Five more, fully specified, so first_mile does not also fire.
	for i := 0; i < 5; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{
			Name: "Fine item", CategoryID: &category.ID, ItemType: store.ItemNonPerishable,
			MinStock: 1, IconName: ptrString("box"),
		})
		require.NoError(t, err)
	}

	holidayMember := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, holidayMember))
	insertHolidayWeek(t, ctx, holidayMember, mostRecentMonday(t, time.Now()))

	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, time.Now()))
	require.Equal(t, 3, countQuests(t, ctx, storageID),
		"uncategorized, imageless and untracked_reorder must all fire — the scenario this test needs")

	alice := newUser(t, ctx)
	require.NoError(t, s.AddMember(ctx, storageID, alice))
	require.NoError(t, s.SetProductCategoryAsUser(ctx, storageID, blank[0], &category.ID, alice))

	isContributor := func(generator string) bool {
		return countRows(t, ctx, `
			SELECT count(*) FROM quest_contributors qc
			  JOIN quests q ON q.id = qc.quest_id
			 WHERE q.storage_id = $1 AND q.generator = $2 AND qc.user_id = $3`,
			storageID, generator, alice) > 0
	}
	assert.True(t, isContributor("uncategorized"), "the field she actually filled")
	assert.False(t, isContributor("imageless"), "she never touched the image")
	assert.False(t, isContributor("untracked_reorder"), "she never touched min_stock")
}

// TestQuestsForStorageIsComebackAfterAGap: quests reappearing after two
// consecutive all-clear weeks must be flagged as a comeback
// (docs/specs/52-gamification-quests-and-ui.md's "coming back from a quiet
// stretch" — the small "new this week" dot).
func TestQuestsForStorageIsComebackAfterAGap(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	for i := 0; i < 6; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Mystery item"})
		require.NoError(t, err)
	}
	// No quests exist for either of the two prior weeks at all — the same
	// state a storage that has been genuinely all-clear for a while is in.

	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, time.Now()))

	snap, err := s.QuestsForStorage(ctx, storageID)
	require.NoError(t, err)
	require.NotEmpty(t, snap.Quests)
	assert.True(t, snap.IsComeback, "quests reappearing with no rows in either prior week must be flagged as a comeback")
}

// TestQuestsForStorageIsNotComebackWhenLastWeekAlsoHadQuests: continuity, not
// novelty — a storage with quests every week is not "coming back" from
// anything.
func TestQuestsForStorageIsNotComebackWhenLastWeekAlsoHadQuests(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	for i := 0; i < 6; i++ {
		_, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Mystery item"})
		require.NoError(t, err)
	}

	thisWeek := mostRecentMonday(t, time.Now())
	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, thisWeek.AddDate(0, 0, -7)))
	require.NoError(t, s.GenerateWeeklyQuests(ctx, storageID, thisWeek))

	snap, err := s.QuestsForStorage(ctx, storageID)
	require.NoError(t, err)
	require.NotEmpty(t, snap.Quests)
	assert.False(t, snap.IsComeback, "quests present last week too must not be flagged as a comeback")
}
