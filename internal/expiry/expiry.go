// Package expiry resolves how long something keeps, and recomputes the dates
// that follow from that answer
// (docs/specs/08-expiration-and-classification.md).
//
// # Shelf-life rules are data, not code
//
// Almost every number this package uses comes from the database — a category's
// default_shelf_life_days, a product's local override, an admin-curated value
// on the catalog entry. A household can change any of them in the app without
// a code change or a redeploy, which is the whole design: "dairy keeps 10
// days" is an opinion about someone's fridge, not a fact to compile in.
//
// The one exception is the item-type fallback at the end of the chain, which
// exists so that a product with no category and no rule anywhere still gets a
// sensible answer rather than an arbitrary one. For a perishable or a
// long-shelf-life item that answer is a date; for a non-perishable it is "no
// expiration", which is equally an answer — a plush toy does not go off.
//
// # Derived versus user, and why the distinction carries the package
//
// Every batch records whether its date came from these rules ('derived') or
// from a person ('user'). Recomputation only ever touches derived dates. A
// date somebody read off a package and typed in outranks every rule here,
// permanently — including the deliberate statement "this has no expiry at
// all", which is a NULL date with a 'user' source and must survive every
// cascade untouched.
//
// Getting that backwards is the failure this package is shaped to prevent: a
// user who clears the date on a jar with no printed date, and finds the system
// has helpfully put one back, has learned that the app ignores them.
package expiry

import (
	"time"
)

// ItemType mirrors products.item_type. It is declared here rather than
// imported from the store so this package stays pure — the resolution chain is
// arithmetic over values, and keeping it free of database types is what lets
// it be tested exhaustively without one.
type ItemType string

const (
	Perishable    ItemType = "perishable"
	LongShelfLife ItemType = "long_shelf_life"
	NonPerishable ItemType = "non_perishable"
)

// itemTypeFallbackDays is the final fallback, used only when nothing in the
// database defines a shelf life for a product — including when it has no
// category at all.
//
// A nil value means "no expiration", which is a real answer rather than a
// missing one: a plush toy does not go off.
var itemTypeFallbackDays = map[ItemType]*int{
	Perishable:    intPtr(7),
	LongShelfLife: intPtr(365),
	NonPerishable: nil,
}

func intPtr(n int) *int { return &n }

// Rules is everything the resolution chain may consult for one product,
// gathered by the caller.
//
// The fields are in priority order, which is not a coincidence: reading this
// struct top to bottom is reading the chain in
// docs/specs/08-expiration-and-classification.md.
type Rules struct {
	// ProductDays is this storage's own override for this product.
	ProductDays *int
	// CatalogDays is the admin-curated value on the catalog entry the product
	// came from.
	CatalogDays *int
	// CategoryDays is the product's own category, then each ancestor walking
	// up toward the root, in that order.
	CategoryDays []*int
	// ItemType is consulted only when everything above is nil.
	ItemType ItemType
}

// Resolution is the outcome of the chain.
type Resolution struct {
	// Days is the shelf life in days, or nil for "no expiration".
	Days *int
	// Source names which rule supplied the answer. It exists for the admin
	// UI and for debugging a date somebody disagrees with — "why is this 10
	// days?" is otherwise an archaeology exercise across four tables.
	Source Source
}

// Source identifies which rule in the chain produced a resolution.
type Source string

const (
	SourceProduct      Source = "product"
	SourceCatalog      Source = "catalog"
	SourceCategory     Source = "category"
	SourceItemType     Source = "item_type"
	SourceUnclassified Source = "unclassified"
)

// Resolve walks the chain and returns the first rule that defines a value.
//
// "Defines a value" means the pointer is non-nil. A category that explicitly
// holds NULL does not stop the walk — NULL means "inherit", which is why
// `Household → NULL` in the starter tree lets its children decide rather than
// forcing every household item to have no expiry.
func Resolve(rules Rules) Resolution {
	if rules.ProductDays != nil {
		return Resolution{Days: rules.ProductDays, Source: SourceProduct}
	}
	if rules.CatalogDays != nil {
		return Resolution{Days: rules.CatalogDays, Source: SourceCatalog}
	}
	for _, days := range rules.CategoryDays {
		if days != nil {
			return Resolution{Days: days, Source: SourceCategory}
		}
	}

	fallback, known := itemTypeFallbackDays[rules.ItemType]
	if !known {
		// An item_type the map does not know is a data error rather than a
		// shelf life. Answering "no expiration" is the safe direction: it
		// leaves the field blank for a person to fill instead of inventing a
		// date nobody chose.
		return Resolution{Days: nil, Source: SourceUnclassified}
	}
	return Resolution{Days: fallback, Source: SourceItemType}
}

// DateFor turns a resolution into the date a batch created at createdAt should
// carry, or nil for "no expiration".
//
// The arithmetic is on calendar days in UTC, deliberately. A shelf life is "ten
// days from when I put it in the fridge", not an instant: carrying the
// creation time of day through would make two batches added an hour apart
// expire on different dates, and make the answer depend on the server's
// timezone.
func DateFor(createdAt time.Time, resolution Resolution) *time.Time {
	if resolution.Days == nil {
		return nil
	}
	day := createdAt.UTC().Truncate(24 * time.Hour)
	expires := day.AddDate(0, 0, *resolution.Days)
	return &expires
}

// Urgency is how close a batch is to being unusable, and is the one
// classification in the system (docs/specs/08-expiration-and-classification.md's
// "Sorting & filtering by urgency" table).
//
// It lives here rather than in whatever surface needs it first because the
// digest of docs/specs/17-expiry-notifications.md and the dashboard must
// bucket the same batch the same way at the same instant. Two
// implementations of "within three days" agree until one of them is changed,
// and then they disagree silently — a person reads "expires tomorrow" on one
// screen and nothing on the other, and neither is obviously wrong.
type Urgency string

const (
	// UrgencyExpired is a date already past: expiration_date < today.
	UrgencyExpired Urgency = "expired"
	// UrgencyCritical is today up to, but not including, three days out.
	UrgencyCritical Urgency = "critical"
	// UrgencySoon is three days out up to, but not including, fourteen.
	UrgencySoon Urgency = "soon"
	// UrgencyOK is fourteen days out or further.
	UrgencyOK Urgency = "ok"
	// UrgencyNone is a batch with no expiration date at all — including the
	// deliberate "this has no expiry" a person recorded, which is a NULL date
	// with a 'user' source and is not a missing value.
	UrgencyNone Urgency = "none"
)

// Urgency window boundaries, in days from today. Exported because the digest
// needs to ask the database for "everything that could possibly be in a
// bucket" before bucketing in Go, and hardcoding 14 at that call site would
// be the second definition this type exists to prevent.
const (
	// CriticalWithinDays ends the critical band and starts the soon band.
	CriticalWithinDays = 3
	// SoonWithinDays ends the soon band. A date on or after this is OK.
	SoonWithinDays = 14
)

// Classify buckets one expiration date relative to today.
//
// Both arguments are read as calendar days; the time of day on either is
// ignored. Which day "today" is remains the caller's decision: the scheduler
// uses the server's local day (docs/specs/17-expiry-notifications.md, and the
// tzdata shipped per docs/specs/01-architecture-and-deployment.md); letting
// this package call time.Now itself would bury that decision where nobody
// looking at a wrong-day bug would think to check.
//
// The bands are half-open and contiguous, exactly as the spec's table states
// them, so every date falls in exactly one: a batch expiring in exactly three
// days is soon, not critical, and one expiring in exactly fourteen is ok, not
// soon.
func Classify(date *time.Time, today time.Time) Urgency {
	if date == nil {
		return UrgencyNone
	}
	day := calendarDay(*date)
	start := calendarDay(today)

	switch {
	case day.Before(start):
		return UrgencyExpired
	case day.Before(start.AddDate(0, 0, CriticalWithinDays)):
		return UrgencyCritical
	case day.Before(start.AddDate(0, 0, SoonWithinDays)):
		return UrgencySoon
	default:
		return UrgencyOK
	}
}

// calendarDay reduces an instant to the calendar day it names, in one common
// frame so two values from different zones can be compared at all.
//
// Both halves of a classification are calendar days, but they do not arrive
// in the same location: an expiration DATE read by pgx is midnight UTC, while
// "today" comes from time.Now() in the server's zone. Comparing those as
// instants gets the answer wrong by a day for every zone west of UTC — with
// the server at UTC-5, midnight local is 05:00 UTC, so a batch expiring today
// would sort before it and be reported as already expired. Rebasing both onto
// UTC midnight compares the dates people mean rather than the moments the
// values happen to hold.
//
// Truncate(24*time.Hour) is also deliberately not used: it truncates against
// the Unix epoch, which is only the start of a day for values already in UTC.
func calendarDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
