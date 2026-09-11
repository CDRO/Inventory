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
// exists only so that a product with no category and no rule anywhere still
// gets a sensible date instead of none.
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
