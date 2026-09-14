package gamification_test

import (
	"testing"

	"github.com/CDRO/Inventory/internal/gamification"
)

func TestSelectQuestsNeverPadsBelowThree(t *testing.T) {
	candidates := []gamification.Candidate{
		{Generator: gamification.GeneratorUncategorized, Fires: true, Need: 5},
		{Generator: gamification.GeneratorImageless, Fires: false, Need: 99},
	}

	got := gamification.SelectQuests(candidates)
	if len(got) != 1 {
		t.Fatalf("SelectQuests() = %d quests, want 1 (one fired, one did not, never padded)", len(got))
	}
	if got[0].Generator != gamification.GeneratorUncategorized {
		t.Errorf("SelectQuests()[0].Generator = %q, want the one candidate that fired", got[0].Generator)
	}
}

func TestSelectQuestsZeroFiredIsAllClear(t *testing.T) {
	candidates := []gamification.Candidate{
		{Generator: gamification.GeneratorUncategorized, Fires: false, Need: 500},
		{Generator: gamification.GeneratorImageless, Fires: false, Need: 500},
	}

	got := gamification.SelectQuests(candidates)
	if len(got) != 0 {
		t.Fatalf("SelectQuests() = %d quests, want 0 (all-clear state, not an empty-list bug)", len(got))
	}
}

func TestSelectQuestsPicksTopThreeByNeed(t *testing.T) {
	candidates := []gamification.Candidate{
		{Generator: gamification.GeneratorStaleLocation, Fires: true, Need: 10},
		{Generator: gamification.GeneratorMissingExpiry, Fires: true, Need: 90},
		{Generator: gamification.GeneratorUncategorized, Fires: true, Need: 50},
		{Generator: gamification.GeneratorImageless, Fires: true, Need: 70},
		{Generator: gamification.GeneratorUntrackedReorder, Fires: true, Need: 30},
	}

	got := gamification.SelectQuests(candidates)
	if len(got) != 3 {
		t.Fatalf("SelectQuests() = %d quests, want 3", len(got))
	}
	want := []gamification.GeneratorKind{
		gamification.GeneratorMissingExpiry, // 90
		gamification.GeneratorImageless,     // 70
		gamification.GeneratorUncategorized, // 50
	}
	for i, g := range want {
		if got[i].Generator != g {
			t.Errorf("SelectQuests()[%d].Generator = %q, want %q", i, got[i].Generator, g)
		}
	}
}

// A tie in need score must resolve the same way every run — otherwise which
// three quests a storage sees on a given Monday would depend on map
// iteration order upstream, and the same live data could offer a different
// quest set on a re-run.
func TestSelectQuestsTiesBreakByInputOrder(t *testing.T) {
	candidates := []gamification.Candidate{
		{Generator: gamification.GeneratorStaleLocation, Fires: true, Need: 10},
		{Generator: gamification.GeneratorMissingExpiry, Fires: true, Need: 10},
		{Generator: gamification.GeneratorUncategorized, Fires: true, Need: 10},
		{Generator: gamification.GeneratorImageless, Fires: true, Need: 10},
	}

	first := gamification.SelectQuests(candidates)
	second := gamification.SelectQuests(candidates)
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("SelectQuests() returned %d and %d quests, want 3 and 3", len(first), len(second))
	}
	for i := range first {
		if first[i].Generator != second[i].Generator {
			t.Fatalf("SelectQuests() is not deterministic on ties: run 1 = %q, run 2 = %q at index %d",
				first[i].Generator, second[i].Generator, i)
		}
	}
	// And it must match the input order specifically, not just be internally
	// consistent with itself.
	wantOrder := []gamification.GeneratorKind{
		gamification.GeneratorStaleLocation,
		gamification.GeneratorMissingExpiry,
		gamification.GeneratorUncategorized,
	}
	for i, g := range wantOrder {
		if first[i].Generator != g {
			t.Errorf("SelectQuests()[%d].Generator = %q, want %q (input order)", i, first[i].Generator, g)
		}
	}
}

func TestXPForGeneratorMatchesSpecTable(t *testing.T) {
	cases := map[gamification.GeneratorKind]int{
		gamification.GeneratorStaleLocation:      40,
		gamification.GeneratorMissingExpiry:      25,
		gamification.GeneratorUncategorized:      20,
		gamification.GeneratorImageless:          20,
		gamification.GeneratorUntrackedReorder:   20,
		gamification.GeneratorExpiringSoon:       30,
		gamification.GeneratorConsumptionHygiene: 15,
		gamification.GeneratorFirstMile:          40,
	}
	for generator, want := range cases {
		if got := gamification.XPForGenerator(generator); got != want {
			t.Errorf("XPForGenerator(%q) = %d, want %d", generator, got, want)
		}
	}
}

func TestGeneratorPriorityOrdersBySpecTable(t *testing.T) {
	if gamification.GeneratorPriority(gamification.GeneratorStaleLocation) >=
		gamification.GeneratorPriority(gamification.GeneratorFirstMile) {
		t.Errorf("GeneratorPriority: stale_location should sort before first_mile, matching the spec table order")
	}
}
