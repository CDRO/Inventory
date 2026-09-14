package gamification

import "fmt"

// AchievementKey identifies one achievement
// (docs/specs/52-gamification-quests-and-ui.md), matching
// achievements_unlocked.achievement_key exactly.
type AchievementKey string

const (
	AchFirstShelf    AchievementKey = "first_shelf"
	AchCartographer  AchievementKey = "cartographer"
	AchCurator       AchievementKey = "curator"
	AchLibrarian     AchievementKey = "librarian"
	AchArchivist     AchievementKey = "archivist"
	AchZeroWasteWeek AchievementKey = "zero_waste_week"
	AchWellStocked   AchievementKey = "well_stocked"
	AchDeepFreeze    AchievementKey = "deep_freeze"
	AchSteadyHand    AchievementKey = "steady_hand"
	AchSteadyHand13  AchievementKey = "steady_hand_13"
	AchSteadyHand26  AchievementKey = "steady_hand_26"
	AchSteadyHand39  AchievementKey = "steady_hand_39"
	AchSteadyHand52  AchievementKey = "steady_hand_52"
	AchSpringClean   AchievementKey = "spring_clean"
)

// Named thresholds behind the achievements above
// (docs/specs/52-gamification-quests-and-ui.md), rather than literals
// scattered across the predicates that check them.
const (
	CuratorCorrections        = 25
	CartographerTreeDepth     = 3
	LibrarianHealthScore      = 80.0
	ArchivistHealthScore      = 95.0
	DeepFreezeProductCount    = 100
	WellStockedDays           = 7
	SpringCleanMinCategorized = 100
)

// steadyHandTier is one rung of the streak-length ladder. The tiers are
// cumulative milestones on one counter, not five separate mechanics: a user
// passing 52 weeks has already collected the four below it
// (docs/specs/52-gamification-quests-and-ui.md).
type steadyHandTier struct {
	Weeks int
	Key   AchievementKey
}

var steadyHandTiers = []steadyHandTier{
	{4, AchSteadyHand},
	{13, AchSteadyHand13},
	{26, AchSteadyHand26},
	{39, AchSteadyHand39},
	{52, AchSteadyHand52},
}

// SteadyHandTiersReached returns every steady_hand tier key a streak of this
// many weeks has earned, in ascending order.
func SteadyHandTiersReached(streakWeeks int) []AchievementKey {
	var out []AchievementKey
	for _, tier := range steadyHandTiers {
		if streakWeeks >= tier.Weeks {
			out = append(out, tier.Key)
		}
	}
	return out
}

// ImmaculateMilestone is one clean-storage milestone key and the XP it pays
// every member of the storage.
type ImmaculateMilestone struct {
	Key string
	XP  int
}

// immaculateFixed is the four named clean-storage milestones
// (docs/specs/52-gamification-quests-and-ui.md). Beyond the last of these,
// ImmaculateMilestonesReached generates immaculate_year_N milestones every
// further 365 days.
var immaculateFixed = []struct {
	Days int
	Key  string
	XP   int
}{
	{90, "immaculate_90", 200},
	{180, "immaculate_180", 500},
	{365, "immaculate_365", 1200},
	{730, "immaculate_730", 3000},
}

// immaculateYearXP is what every immaculate_year_N milestone beyond
// immaculate_730 pays.
const immaculateYearXP = 3000

// ImmaculateMilestonesReached returns every clean-storage milestone
// (docs/specs/52-gamification-quests-and-ui.md) that cleanDays of
// unbroken clean_since has earned, in ascending order. N in
// "immaculate_year_N" counts the further-365-day milestones themselves
// (N=1 at day 1095, the first full year beyond immaculate_730), not
// calendar years since clean_since began.
func ImmaculateMilestonesReached(cleanDays int) []ImmaculateMilestone {
	var out []ImmaculateMilestone
	for _, m := range immaculateFixed {
		if cleanDays >= m.Days {
			out = append(out, ImmaculateMilestone{Key: m.Key, XP: m.XP})
		}
	}
	if cleanDays > immaculateFixed[len(immaculateFixed)-1].Days {
		lastDays := immaculateFixed[len(immaculateFixed)-1].Days
		extraYears := (cleanDays - lastDays) / 365
		for y := 1; y <= extraYears; y++ {
			out = append(out, ImmaculateMilestone{
				Key: fmt.Sprintf("immaculate_year_%d", y),
				XP:  immaculateYearXP,
			})
		}
	}
	return out
}
