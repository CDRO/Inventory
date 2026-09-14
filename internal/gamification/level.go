package gamification

import "math"

// Level converts total XP into a level: fast early levels for a new user's
// first session, slowing steadily, with no cap, no prestige, and no decay
// (docs/specs/51-gamification-scoring.md).
func Level(xp int) int {
	if xp < 0 {
		xp = 0
	}
	return int(math.Floor(math.Sqrt(float64(xp)/50))) + 1
}

// XPThresholdForLevel is Level's inverse: the XP at which a user first
// reaches level, so `Level(XPThresholdForLevel(n)) == n` for every n >= 1.
//
// docs/specs/52-gamification-quests-and-ui.md's header-ring popover
// ("Progress to level 7: 12/250 XP") needs both the current and next
// level's threshold "computed server-side and returned alongside the level
// ... so the frontend never re-implements the curve" — this is that
// computation, kept next to Level so the two can never drift apart.
func XPThresholdForLevel(level int) int {
	if level < 1 {
		level = 1
	}
	step := level - 1
	return 50 * step * step
}
