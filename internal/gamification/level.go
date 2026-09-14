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
