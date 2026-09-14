package gamification_test

import (
	"testing"
	"time"

	"github.com/CDRO/Inventory/internal/gamification"
)

func monday(offsetWeeks int) time.Time {
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // a Monday
	return base.AddDate(0, 0, offsetWeeks*7)
}

func TestMaxWeeksInWindowEmpty(t *testing.T) {
	if got := gamification.MaxWeeksInWindow(nil); got != 0 {
		t.Errorf("MaxWeeksInWindow(nil) = %d, want 0", got)
	}
}

func TestMaxWeeksInWindowUnderBudget(t *testing.T) {
	weeks := []time.Time{monday(0), monday(1), monday(2)}
	if got := gamification.MaxWeeksInWindow(weeks); got != 3 {
		t.Errorf("MaxWeeksInWindow(3 adjacent weeks) = %d, want 3", got)
	}
}

// Eight weeks scattered across exactly one year apart from the first still
// all fall inside one 52-week window — the budget is "at most 8 in ANY
// rolling window", not "at most 8 per calendar year".
func TestMaxWeeksInWindowScatteredWithinOneWindow(t *testing.T) {
	weeks := []time.Time{monday(0), monday(10), monday(20), monday(30), monday(40), monday(51)}
	if got := gamification.MaxWeeksInWindow(weeks); got != 6 {
		t.Errorf("MaxWeeksInWindow(6 weeks spanning 51 weeks) = %d, want 6 (all one window)", got)
	}
}

// Two clusters of weeks more than 52 weeks apart must not be summed into one
// window just because both exist — the rolling window is the max over every
// possible 52-week span, not the count across all of history.
func TestMaxWeeksInWindowSeparateClustersNeverSum(t *testing.T) {
	weeks := []time.Time{
		monday(0), monday(1), monday(2), monday(3), monday(4), // 5 weeks, one trip
		monday(100), monday(101), monday(102), monday(103), monday(104), // 5 weeks, a different year entirely
	}
	if got := gamification.MaxWeeksInWindow(weeks); got != 5 {
		t.Errorf("MaxWeeksInWindow(two distant 5-week clusters) = %d, want 5 (no window contains both clusters)", got)
	}
}

func TestMaxWeeksInWindowExactlyAtBoundary(t *testing.T) {
	// monday(0) and monday(51) are 51 weeks apart — inside one 52-week window.
	// monday(0) and monday(52) are 52 weeks apart — that already spans two
	// windows of 52 consecutive weeks (indices 0..51 and 1..52), so they can
	// never both sit inside one.
	inside := gamification.MaxWeeksInWindow([]time.Time{monday(0), monday(51)})
	if inside != 2 {
		t.Errorf("MaxWeeksInWindow(51 weeks apart) = %d, want 2 (fits in one window)", inside)
	}
	outside := gamification.MaxWeeksInWindow([]time.Time{monday(0), monday(52)})
	if outside != 1 {
		t.Errorf("MaxWeeksInWindow(52 weeks apart) = %d, want 1 (no single window holds both)", outside)
	}
}

func TestMaxWeeksInWindowOrderIndependent(t *testing.T) {
	weeks := []time.Time{monday(5), monday(0), monday(3), monday(1), monday(2)}
	if got := gamification.MaxWeeksInWindow(weeks); got != 5 {
		t.Errorf("MaxWeeksInWindow(unsorted input) = %d, want 5", got)
	}
}
