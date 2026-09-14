package gamification_test

import (
	"reflect"
	"testing"

	"github.com/CDRO/Inventory/internal/gamification"
)

func TestSteadyHandTiersReachedIsCumulative(t *testing.T) {
	cases := []struct {
		weeks int
		want  []gamification.AchievementKey
	}{
		{0, nil},
		{3, nil},
		{4, []gamification.AchievementKey{gamification.AchSteadyHand}},
		{13, []gamification.AchievementKey{gamification.AchSteadyHand, gamification.AchSteadyHand13}},
		{51, []gamification.AchievementKey{gamification.AchSteadyHand, gamification.AchSteadyHand13, gamification.AchSteadyHand26, gamification.AchSteadyHand39}},
		{52, []gamification.AchievementKey{
			gamification.AchSteadyHand, gamification.AchSteadyHand13, gamification.AchSteadyHand26,
			gamification.AchSteadyHand39, gamification.AchSteadyHand52,
		}},
		// A streak taken with the full 8-week holiday budget can span up to 60
		// calendar weeks without breaking, so a streak counter well past 52 must
		// still only report the tiers that actually exist.
		{60, []gamification.AchievementKey{
			gamification.AchSteadyHand, gamification.AchSteadyHand13, gamification.AchSteadyHand26,
			gamification.AchSteadyHand39, gamification.AchSteadyHand52,
		}},
	}
	for _, c := range cases {
		got := gamification.SteadyHandTiersReached(c.weeks)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SteadyHandTiersReached(%d) = %v, want %v", c.weeks, got, c.want)
		}
	}
}

func TestImmaculateMilestonesReachedFixedTiers(t *testing.T) {
	cases := []struct {
		days int
		want []string
	}{
		{89, nil},
		{90, []string{"immaculate_90"}},
		{179, []string{"immaculate_90"}},
		{365, []string{"immaculate_90", "immaculate_180", "immaculate_365"}},
		{729, []string{"immaculate_90", "immaculate_180", "immaculate_365"}},
		{730, []string{"immaculate_90", "immaculate_180", "immaculate_365", "immaculate_730"}},
	}
	for _, c := range cases {
		got := gamification.ImmaculateMilestonesReached(c.days)
		var gotKeys []string
		for _, m := range got {
			gotKeys = append(gotKeys, m.Key)
		}
		if !reflect.DeepEqual(gotKeys, c.want) {
			t.Errorf("ImmaculateMilestonesReached(%d) keys = %v, want %v", c.days, gotKeys, c.want)
		}
	}
}

func TestImmaculateMilestonesReachedYearlyBeyondFixedTiers(t *testing.T) {
	// One day short of a further year: still only the four fixed milestones.
	got := gamification.ImmaculateMilestonesReached(730 + 364)
	if len(got) != 4 {
		t.Fatalf("ImmaculateMilestonesReached(1094) = %d milestones, want 4 (no year-N yet)", len(got))
	}

	got = gamification.ImmaculateMilestonesReached(730 + 365)
	if len(got) != 5 || got[4].Key != "immaculate_year_1" {
		t.Fatalf("ImmaculateMilestonesReached(1095) = %v, want a 5th milestone immaculate_year_1", got)
	}
	if got[4].XP != 3000 {
		t.Errorf("immaculate_year_1 XP = %d, want 3000", got[4].XP)
	}

	got = gamification.ImmaculateMilestonesReached(730 + 365*2)
	lastKey := got[len(got)-1].Key
	if lastKey != "immaculate_year_2" {
		t.Errorf("ImmaculateMilestonesReached(730+730) last key = %q, want immaculate_year_2", lastKey)
	}
}

// Every member of the storage is paid for a clean-storage milestone, so a
// caller must be able to tell exactly which *new* milestones a clean_since
// update crossed, not just which are reached in total — otherwise a
// recompute would re-pay members who already have immaculate_90 every time
// it also reaches immaculate_180.
func TestImmaculateMilestonesReachedIsPrefixMonotonic(t *testing.T) {
	at90 := gamification.ImmaculateMilestonesReached(90)
	at365 := gamification.ImmaculateMilestonesReached(365)
	if len(at365) < len(at90) {
		t.Fatalf("more clean days must never reach fewer milestones: got %d at 365 days, %d at 90", len(at365), len(at90))
	}
	for i := range at90 {
		if at90[i].Key != at365[i].Key {
			t.Errorf("ImmaculateMilestonesReached(365)[%d] = %q, want %q (365 days' result must extend 90 days', not reorder it)",
				i, at365[i].Key, at90[i].Key)
		}
	}
}
