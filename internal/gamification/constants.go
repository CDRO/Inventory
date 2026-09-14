package gamification

// ContributionKind is the set of scoreable actions that inventory_logs does
// not itself capture (docs/specs/51-gamification-scoring.md). The values
// match the CHECK on contribution_events.kind exactly.
type ContributionKind string

const (
	KindAICorrection      ContributionKind = "ai_correction"
	KindMetadataFilled    ContributionKind = "metadata_filled"
	KindLocationMapped    ContributionKind = "location_mapped"
	KindCategoryCreated   ContributionKind = "category_created"
	KindExpiryConfirmed   ContributionKind = "expiry_confirmed"
	KindAmbiguityResolved ContributionKind = "ambiguity_resolved"
)

// XP values, named per docs/specs/51-gamification-scoring.md's table rather
// than scattered as literals.
const (
	// XPLedgerContribution is what a distinct product earns from
	// inventory_logs.reason = 'purchase', 'consumption' or 'vision_ingestion'
	// — the same value for all three, deliberately: consumption reduces the
	// inventory and must be worth exactly as much as adding.
	XPLedgerContribution = 3

	XPAICorrection      = 5
	XPAmbiguityResolved = 4
	XPMetadataFilled    = 2
	XPExpiryConfirmed   = 2
	XPLocationMapped    = 5

	// XPCategoryCreated has no row of its own in the spec's XP table, which
	// lists 'metadata_filled' as covering "category, image, min_stock" and
	// separately enumerates 'category_created' only in the contribution_events
	// CHECK. Creating a new category node is the same shape of gap-filling
	// work as filling in a product's category, so it is scored the same until
	// the spec says otherwise.
	XPCategoryCreated = XPMetadataFilled
)

// scoringLedgerReasons is the inventory_logs.reason values that earn XP. The
// other two reasons the CHECK allows — 'audit' and 'move' — do not: an audit
// row corrects the ledger rather than recording new work, and a move
// relocates stock without adding, removing, or improving anything about it.
var scoringLedgerReasons = map[string]bool{
	"purchase":         true,
	"consumption":      true,
	"vision_ingestion": true,
}

// XPForLedgerReason returns the XP a distinct product earns for one
// inventory_logs row with this reason, and whether the reason scores at all.
func XPForLedgerReason(reason string) (xp int, ok bool) {
	if scoringLedgerReasons[reason] {
		return XPLedgerContribution, true
	}
	return 0, false
}

// XPForContribution returns the XP one contribution_events row of this kind
// is worth.
func XPForContribution(kind ContributionKind) int {
	switch kind {
	case KindAICorrection:
		return XPAICorrection
	case KindAmbiguityResolved:
		return XPAmbiguityResolved
	case KindMetadataFilled:
		return XPMetadataFilled
	case KindExpiryConfirmed:
		return XPExpiryConfirmed
	case KindLocationMapped:
		return XPLocationMapped
	case KindCategoryCreated:
		return XPCategoryCreated
	default:
		return 0
	}
}
