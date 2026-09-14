package gamification

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// ScoreWindow is the coalescing window: two events for the same (ref, kind)
// less than this far apart score as one contribution, not two
// (docs/specs/51-gamification-scoring.md). Taking three yoghurts out one at a
// time over a morning is one act of logging; the same product again that
// evening opens a new window and scores again.
const ScoreWindow = 2 * time.Hour

// Event is one raw, timestamped occurrence to coalesce and score — either an
// inventory_logs row (Kind is its reason) or a contribution_events row (Kind
// is its ContributionKind, as a string).
type Event struct {
	// RefID is the product, location or batch the event applies to: the same
	// entity named by inventory_logs.product_id or contribution_events.ref_id.
	RefID uuid.UUID
	Kind  string
	At    time.Time
	// XP is what one occurrence of this kind is worth, looked up by the
	// caller via XPForLedgerReason or XPForContribution before building the
	// Event — this package scores windows, not kinds.
	XP int
}

// Coalesce groups events by (RefID, Kind), splits each group into windows
// separated by gaps of ScoreWindow or more, and sums one contribution's XP
// per window rather than per event.
//
// This is also what makes "create-delete cycles earn nothing"
// (docs/specs/51-gamification-scoring.md) require no extra bookkeeping:
// Coalesce is handed the *current* ledger state, and inventory_logs rows
// cascade away with the product they belong to (docs/specs/02-data-model.md).
// A deleted product's events are simply absent from the next call, so
// recomputing from scratch already reflects the deletion — there is nothing
// to separately track or subtract.
//
// vision_ingestion and ai_correction are coalesced into one group per
// product rather than kept apart by kind, because they are two possible
// outcomes of the same review action — confirming one detected row, either
// as-is or corrected. Scoring both in the same window would double-count a
// single decision; "correcting the AI is worth more than accepting it (5 vs
// 3)" describes a comparison between the two outcomes, not a bonus stacked
// on top of the base credit, so the window scores its highest-value event
// only.
func Coalesce(events []Event) int {
	type key struct {
		ref  uuid.UUID
		kind string
	}
	groups := make(map[key][]Event, len(events))
	for _, e := range events {
		k := key{ref: e.RefID, kind: coalesceGroup(e.Kind)}
		groups[k] = append(groups[k], e)
	}

	total := 0
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].At.Before(g[j].At) })

		windowStart := 0
		for i := 1; i <= len(g); i++ {
			if i == len(g) || g[i].At.Sub(g[i-1].At) >= ScoreWindow {
				total += maxXP(g[windowStart:i])
				windowStart = i
			}
		}
	}
	return total
}

// confirmActionGroup is the shared coalescing key for vision_ingestion and
// ai_correction. Any string works as long as it collides with neither real
// kind; ledger reasons and contribution kinds never contain a space.
const confirmActionGroup = "confirm action"

// coalesceGroup returns the key events of this kind coalesce under. See
// Coalesce's doc comment for why vision_ingestion and ai_correction share
// one.
func coalesceGroup(kind string) string {
	switch kind {
	case "vision_ingestion", string(KindAICorrection):
		return confirmActionGroup
	default:
		return kind
	}
}

func maxXP(events []Event) int {
	highest := 0
	for _, e := range events {
		if e.XP > highest {
			highest = e.XP
		}
	}
	return highest
}
