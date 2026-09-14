package gamification_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/CDRO/Inventory/internal/gamification"
)

func TestLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		xp   int
		want int
	}{
		{"zero XP is level 1", 0, 1},
		{"just under the first threshold stays level 1", 49, 1},
		{"exactly the first threshold reaches level 2", 50, 2},
		{"200 XP is level 3", 200, 3},
		{"450 XP is level 4", 450, 4},
		{"negative XP (should never happen) clamps to level 1, not a crash or a negative level", -10, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, gamification.Level(tc.xp))
		})
	}
}

// TestXPThresholdForLevelInvertsLevel checks the two functions agree at
// every level's boundary: the popover's "12/250 XP" is only honest if
// XPThresholdForLevel(n) is exactly the first xp for which Level reports n.
func TestXPThresholdForLevelInvertsLevel(t *testing.T) {
	t.Parallel()

	for level := 1; level <= 20; level++ {
		threshold := gamification.XPThresholdForLevel(level)
		assert.Equal(t, level, gamification.Level(threshold), "Level(XPThresholdForLevel(%d)) must be %d", level, level)
		if threshold > 0 {
			assert.Equal(t, level-1, gamification.Level(threshold-1), "one XP short of the threshold must still be the previous level")
		}
	}
}

func TestXPThresholdForLevelClampsBelowOne(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, gamification.XPThresholdForLevel(0))
	assert.Equal(t, 0, gamification.XPThresholdForLevel(-5))
}

// TestLevelNeverDecreasesWithMoreXP guards the "no decay" invariant
// (docs/specs/51-gamification-scoring.md) at the level-formula boundary: XP
// only ever accumulates, so the function backing it must be monotonic, or a
// future refactor of the formula could silently take someone's level down.
func TestLevelNeverDecreasesWithMoreXP(t *testing.T) {
	t.Parallel()

	prev := gamification.Level(0)
	for xp := 1; xp <= 5000; xp++ {
		level := gamification.Level(xp)
		if level < prev {
			t.Fatalf("Level(%d) = %d, which is lower than Level(%d) = %d", xp, level, xp-1, prev)
		}
		prev = level
	}
}
