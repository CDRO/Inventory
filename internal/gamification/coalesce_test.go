package gamification_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/CDRO/Inventory/internal/gamification"
)

func TestCoalesceSingleEventScoresOnce(t *testing.T) {
	t.Parallel()

	product := uuid.New()
	total := gamification.Coalesce([]gamification.Event{
		{RefID: product, Kind: "consumption", At: time.Now(), XP: 3},
	})
	assert.Equal(t, 3, total)
}

// TestCoalesceRepeatedEventsWithinTheWindowScoreOnce is the core anti-gaming
// rule (docs/specs/51-gamification-scoring.md): taking three yoghurts out one
// at a time over a morning is one act of logging, not three.
func TestCoalesceRepeatedEventsWithinTheWindowScoreOnce(t *testing.T) {
	t.Parallel()

	product := uuid.New()
	start := time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)
	events := []gamification.Event{
		{RefID: product, Kind: "consumption", At: start, XP: 3},
		{RefID: product, Kind: "consumption", At: start.Add(30 * time.Minute), XP: 3},
		{RefID: product, Kind: "consumption", At: start.Add(90 * time.Minute), XP: 3},
	}
	assert.Equal(t, 3, gamification.Coalesce(events), "three events inside one 2-hour window still score once")
}

// TestCoalesceEventsPastTheWindowScoreSeparately is the other half: a
// genuinely separate act, hours later, must open a new window.
func TestCoalesceEventsPastTheWindowScoreSeparately(t *testing.T) {
	t.Parallel()

	product := uuid.New()
	morning := time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)
	evening := morning.Add(9 * time.Hour)
	events := []gamification.Event{
		{RefID: product, Kind: "consumption", At: morning, XP: 3},
		{RefID: product, Kind: "consumption", At: evening, XP: 3},
	}
	assert.Equal(t, 6, gamification.Coalesce(events))
}

// TestCoalesceWindowBoundaryIsExclusive locks down the exact boundary: a gap
// of precisely ScoreWindow opens a new window rather than joining the last
// one, and one gap ScoreWindow-minus-a-nanosecond stays in it. A future
// off-by-one on the comparison operator would only show up at this edge.
func TestCoalesceWindowBoundaryIsExclusive(t *testing.T) {
	t.Parallel()

	product := uuid.New()
	start := time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

	justInside := []gamification.Event{
		{RefID: product, Kind: "consumption", At: start, XP: 3},
		{RefID: product, Kind: "consumption", At: start.Add(gamification.ScoreWindow - time.Nanosecond), XP: 3},
	}
	assert.Equal(t, 3, gamification.Coalesce(justInside), "a gap one nanosecond under the window still coalesces")

	exactlyAt := []gamification.Event{
		{RefID: product, Kind: "consumption", At: start, XP: 3},
		{RefID: product, Kind: "consumption", At: start.Add(gamification.ScoreWindow), XP: 3},
	}
	assert.Equal(t, 6, gamification.Coalesce(exactlyAt), "a gap of exactly the window opens a new one")
}

// TestCoalesceDifferentProductsNeverMix guards against a grouping bug that
// keys on kind alone and merges unrelated products into one window.
func TestCoalesceDifferentProductsNeverMix(t *testing.T) {
	t.Parallel()

	now := time.Now()
	events := []gamification.Event{
		{RefID: uuid.New(), Kind: "consumption", At: now, XP: 3},
		{RefID: uuid.New(), Kind: "consumption", At: now, XP: 3},
	}
	assert.Equal(t, 6, gamification.Coalesce(events))
}

// TestCoalesceDifferentKindsNeverMix: purchase and expiry_confirmed on the
// same product at the same instant are two different contributions, not one.
func TestCoalesceDifferentKindsNeverMix(t *testing.T) {
	t.Parallel()

	product := uuid.New()
	now := time.Now()
	events := []gamification.Event{
		{RefID: product, Kind: "purchase", At: now, XP: 3},
		{RefID: product, Kind: string(gamification.KindExpiryConfirmed), At: now, XP: 2},
	}
	assert.Equal(t, 5, gamification.Coalesce(events))
}

// TestCoalesceCorrectionSupersedesAcceptance is the one explicit exception:
// "correcting the AI is worth more than accepting it (5 vs 3)" describes a
// comparison, not a bonus stacked on the base credit. Both events happen for
// the same reviewed row, so the window must pay 5, not 8.
func TestCoalesceCorrectionSupersedesAcceptance(t *testing.T) {
	t.Parallel()

	product := uuid.New()
	now := time.Now()
	events := []gamification.Event{
		{RefID: product, Kind: "vision_ingestion", At: now, XP: gamification.XPLedgerContribution},
		{RefID: product, Kind: string(gamification.KindAICorrection), At: now, XP: gamification.XPAICorrection},
	}
	assert.Equal(t, gamification.XPAICorrection, gamification.Coalesce(events), "corrected rows pay 5, not 3+5")
}

// TestCoalesceUncorrectedAcceptanceStillPaysTheBaseAmount is the other half
// of the exception above: accepting the AI's proposal as-is, with no
// ai_correction event at all, must still pay its own 3 — the supersede rule
// must not accidentally suppress the ordinary case.
func TestCoalesceUncorrectedAcceptanceStillPaysTheBaseAmount(t *testing.T) {
	t.Parallel()

	product := uuid.New()
	events := []gamification.Event{
		{RefID: product, Kind: "vision_ingestion", At: time.Now(), XP: gamification.XPLedgerContribution},
	}
	assert.Equal(t, gamification.XPLedgerContribution, gamification.Coalesce(events))
}

// TestCoalesceCorrectionOutsideTheWindowScoresSeparately: the confirm-action
// grouping still respects the 2-hour window like any other — a correction
// recorded hours after an unrelated earlier acceptance of the same product
// is a second, separate act, not a retroactive upgrade of the first.
func TestCoalesceCorrectionOutsideTheWindowScoresSeparately(t *testing.T) {
	t.Parallel()

	product := uuid.New()
	morning := time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)
	events := []gamification.Event{
		{RefID: product, Kind: "vision_ingestion", At: morning, XP: gamification.XPLedgerContribution},
		{RefID: product, Kind: string(gamification.KindAICorrection), At: morning.Add(9 * time.Hour), XP: gamification.XPAICorrection},
	}
	assert.Equal(t, gamification.XPLedgerContribution+gamification.XPAICorrection, gamification.Coalesce(events))
}

func TestCoalesceEmptyInputScoresZero(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, gamification.Coalesce(nil))
}
