package gamification

// PointsPerLoggedItem is the base score awarded for logging one item.
const PointsPerLoggedItem = 10

// StreakBonus returns the multiplier for a consecutive-day logging streak.
func StreakBonus(days int) float64 {
	if days >= 30 {
		return 2.0
	}
	if days >= 7 {
		return 1.5
	}
	return 1.0
}
