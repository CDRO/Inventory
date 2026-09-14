package gamification_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/CDRO/Inventory/internal/gamification"
)

func TestHealthScoreIsTheMeanOfFiveSubScores(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 100.0, gamification.HealthScore(100, 100, 100, 100, 100))
	assert.Equal(t, 0.0, gamification.HealthScore(0, 0, 0, 0, 0))
	assert.InDelta(t, 50.0, gamification.HealthScore(100, 0, 50, 25, 75), 0.0001)
}

// TestHealthScoreWeighsEverySubScoreEqually would catch a copy-paste bug that
// dropped or duplicated one of the five arguments — a tautological "it
// returns *a* number" assertion would not.
func TestHealthScoreWeighsEverySubScoreEqually(t *testing.T) {
	t.Parallel()

	base := gamification.HealthScore(0, 0, 0, 0, 0)
	assert.InDelta(t, base+20, gamification.HealthScore(100, 0, 0, 0, 0), 0.0001, "categorized")
	assert.InDelta(t, base+20, gamification.HealthScore(0, 100, 0, 0, 0), 0.0001, "imaged")
	assert.InDelta(t, base+20, gamification.HealthScore(0, 0, 100, 0, 0), 0.0001, "minStockTracked")
	assert.InDelta(t, base+20, gamification.HealthScore(0, 0, 0, 100, 0), 0.0001, "expiryTracked")
	assert.InDelta(t, base+20, gamification.HealthScore(0, 0, 0, 0, 100), 0.0001, "recentlyActive")
}
