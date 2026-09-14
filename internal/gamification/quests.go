package gamification

import "sort"

// GeneratorKind identifies one weekly quest generator
// (docs/specs/52-gamification-quests-and-ui.md). Values match what the
// frontend switches on to build the rendered quest text from generator +
// params, so renaming one is a breaking API change.
type GeneratorKind string

const (
	GeneratorStaleLocation      GeneratorKind = "stale_location"
	GeneratorMissingExpiry      GeneratorKind = "missing_expiry"
	GeneratorUncategorized      GeneratorKind = "uncategorized"
	GeneratorImageless          GeneratorKind = "imageless"
	GeneratorUntrackedReorder   GeneratorKind = "untracked_reorder"
	GeneratorExpiringSoon       GeneratorKind = "expiring_soon"
	GeneratorConsumptionHygiene GeneratorKind = "consumption_hygiene"
	GeneratorFirstMile          GeneratorKind = "first_mile"
)

// Quest XP rewards, named per docs/specs/52-gamification-quests-and-ui.md's
// generator table rather than scattered as literals.
const (
	XPStaleLocation      = 40
	XPMissingExpiry      = 25
	XPUncategorized      = 20
	XPImageless          = 20
	XPUntrackedReorder   = 20
	XPExpiringSoon       = 30
	XPConsumptionHygiene = 15
	XPFirstMile          = 40
)

// generatorOrder is every generator in the spec table's own order. It is
// what makes quest selection deterministic when two candidates tie on need:
// SelectQuests sorts stably, so candidates built in this order break ties in
// this order, every time, regardless of map iteration order upstream.
var generatorOrder = []GeneratorKind{
	GeneratorStaleLocation,
	GeneratorMissingExpiry,
	GeneratorUncategorized,
	GeneratorImageless,
	GeneratorUntrackedReorder,
	GeneratorExpiringSoon,
	GeneratorConsumptionHygiene,
	GeneratorFirstMile,
}

// GeneratorPriority returns g's index in the spec table, for callers that
// need to build candidates in a stable order.
func GeneratorPriority(g GeneratorKind) int {
	for i, k := range generatorOrder {
		if k == g {
			return i
		}
	}
	return len(generatorOrder)
}

// XPForGenerator returns one generator's quest reward.
func XPForGenerator(g GeneratorKind) int {
	switch g {
	case GeneratorStaleLocation:
		return XPStaleLocation
	case GeneratorMissingExpiry:
		return XPMissingExpiry
	case GeneratorUncategorized:
		return XPUncategorized
	case GeneratorImageless:
		return XPImageless
	case GeneratorUntrackedReorder:
		return XPUntrackedReorder
	case GeneratorExpiringSoon:
		return XPExpiringSoon
	case GeneratorConsumptionHygiene:
		return XPConsumptionHygiene
	case GeneratorFirstMile:
		return XPFirstMile
	default:
		return 0
	}
}

// Candidate is one generator's live evaluation against a storage's real data
// (docs/specs/52-gamification-quests-and-ui.md): whether it fires, how badly
// the storage needs it, and the target count and params the frontend needs
// to render its text and track progress. The store package builds these from
// live queries; this package only ranks them.
type Candidate struct {
	Generator   GeneratorKind
	Fires       bool
	Need        float64
	TargetCount int
	// Params is whatever the store package's chosen params shape for this
	// generator is — this package never inspects it, only ranks candidates.
	Params any
}

// SelectQuests picks the fired candidates with the highest need score, never
// more than three. If fewer than three generators fire, it returns fewer —
// it never pads with a candidate that did not fire, because "a quest that
// exists only to be a quest teaches people to ignore quests"
// (docs/specs/52-gamification-quests-and-ui.md).
//
// Ties are broken by each candidate's position in the input slice, via a
// stable sort — callers should build candidates in generatorOrder (see
// GeneratorPriority) so a tie resolves the same way on every run.
func SelectQuests(candidates []Candidate) []Candidate {
	fired := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Fires {
			fired = append(fired, c)
		}
	}
	sort.SliceStable(fired, func(i, j int) bool { return fired[i].Need > fired[j].Need })
	if len(fired) > 3 {
		fired = fired[:3]
	}
	return fired
}
