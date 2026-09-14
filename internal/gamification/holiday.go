package gamification

import (
	"sort"
	"time"
)

// HolidayBudgetWeeks is the maximum number of holiday weeks a user may have
// marked inside any rolling HolidayWindowWeeks-wide window
// (docs/specs/52-gamification-quests-and-ui.md).
const HolidayBudgetWeeks = 8

// HolidayWindowWeeks is the width, in weeks, of the rolling window the
// budget above applies over.
const HolidayWindowWeeks = 52

// MaxWeeksInWindow returns the largest number of weeks, out of weeks (each
// truncated to its own Monday), that fall inside any single
// HolidayWindowWeeks-wide span of consecutive weeks. This is the "any
// rolling 52-week window" the holiday budget is checked against — not a
// fixed calendar window, but the worst case over every possible one.
func MaxWeeksInWindow(weeks []time.Time) int {
	if len(weeks) == 0 {
		return 0
	}

	sorted := make([]time.Time, len(weeks))
	copy(sorted, weeks)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })

	maxCount := 0
	left := 0
	for right := range sorted {
		for weeksBetween(sorted[left], sorted[right]) >= HolidayWindowWeeks {
			left++
		}
		if count := right - left + 1; count > maxCount {
			maxCount = count
		}
	}
	return maxCount
}

// weeksBetween is the number of 7-day steps between two Mondays. Using
// Hours() rather than a calendar diff sidesteps any DST-related off-by-one:
// callers here only ever pass values already truncated to UTC Mondays.
func weeksBetween(a, b time.Time) int {
	return int(b.Sub(a).Hours() / (24 * 7))
}
