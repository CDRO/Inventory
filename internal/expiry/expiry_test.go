package expiry_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/expiry"
)

func days(n int) *int { return &n }

// TestResolveWalksTheChainInOrder pins the priority order from
// docs/specs/08-expiration-and-classification.md. Each case supplies a value at
// one level plus decoys at every lower-priority level, so a chain that
// consulted them out of order would pick a decoy.
func TestResolveWalksTheChainInOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rules      expiry.Rules
		wantDays   *int
		wantSource expiry.Source
	}{
		{
			name: "the storage's own product override wins over everything",
			rules: expiry.Rules{
				ProductDays:  days(3),
				CatalogDays:  days(30),
				CategoryDays: []*int{days(60), days(90)},
				ItemType:     expiry.Perishable,
			},
			wantDays:   days(3),
			wantSource: expiry.SourceProduct,
		},
		{
			name: "the admin-curated catalog value outranks the category",
			rules: expiry.Rules{
				CatalogDays:  days(30),
				CategoryDays: []*int{days(60)},
				ItemType:     expiry.Perishable,
			},
			wantDays:   days(30),
			wantSource: expiry.SourceCatalog,
		},
		{
			name: "the product's own category outranks its ancestors",
			rules: expiry.Rules{
				CategoryDays: []*int{days(10), days(365)},
				ItemType:     expiry.Perishable,
			},
			wantDays:   days(10),
			wantSource: expiry.SourceCategory,
		},
		{
			name: "a NULL category means inherit, not 'no expiry'",
			rules: expiry.Rules{
				// Household → NULL, with a parent that does define one.
				CategoryDays: []*int{nil, days(365)},
				ItemType:     expiry.Perishable,
			},
			wantDays:   days(365),
			wantSource: expiry.SourceCategory,
		},
		{
			name:       "item type is the last resort",
			rules:      expiry.Rules{ItemType: expiry.Perishable},
			wantDays:   days(7),
			wantSource: expiry.SourceItemType,
		},
		{
			name:       "a long-shelf-life item with no rules anywhere",
			rules:      expiry.Rules{ItemType: expiry.LongShelfLife},
			wantDays:   days(365),
			wantSource: expiry.SourceItemType,
		},
		{
			name:       "a non-perishable item resolves to no expiration",
			rules:      expiry.Rules{ItemType: expiry.NonPerishable},
			wantDays:   nil,
			wantSource: expiry.SourceItemType,
		},
		{
			name: "no category at all still reaches the fallback",
			rules: expiry.Rules{
				CategoryDays: nil,
				ItemType:     expiry.LongShelfLife,
			},
			wantDays:   days(365),
			wantSource: expiry.SourceItemType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := expiry.Resolve(tc.rules)

			assert.Equal(t, tc.wantSource, got.Source)
			if tc.wantDays == nil {
				assert.Nil(t, got.Days)
				return
			}
			require.NotNil(t, got.Days)
			assert.Equal(t, *tc.wantDays, *got.Days)
		})
	}
}

// TestResolveTreatsAnUnknownItemTypeAsNoExpiry — an item_type the map does not
// know is a data error, not a shelf life. Leaving the field blank for a person
// to fill beats inventing a date nobody chose.
func TestResolveTreatsAnUnknownItemTypeAsNoExpiry(t *testing.T) {
	t.Parallel()

	got := expiry.Resolve(expiry.Rules{ItemType: expiry.ItemType("something_new")})

	assert.Nil(t, got.Days)
	assert.Equal(t, expiry.SourceUnclassified, got.Source)
}

// TestResolveReportsWhichRuleAnswered — "why is this 10 days?" is otherwise an
// archaeology exercise across four tables.
func TestResolveReportsWhichRuleAnswered(t *testing.T) {
	t.Parallel()

	assert.Equal(t, expiry.SourceCatalog,
		expiry.Resolve(expiry.Rules{CatalogDays: days(30), ItemType: expiry.Perishable}).Source)
}

func TestDateForAddsCalendarDays(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC)

	got := expiry.DateFor(created, expiry.Resolution{Days: days(10)})

	require.NotNil(t, got)
	assert.Equal(t, "2026-03-11", got.Format(time.DateOnly))
}

// TestDateForIgnoresTheTimeOfDay — a shelf life is "ten days from when I put it
// in the fridge", not an instant. Two batches added an hour apart must expire
// on the same date.
func TestDateForIgnoresTheTimeOfDay(t *testing.T) {
	t.Parallel()

	morning := expiry.DateFor(time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC), expiry.Resolution{Days: days(7)})
	evening := expiry.DateFor(time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC), expiry.Resolution{Days: days(7)})

	require.NotNil(t, morning)
	require.NotNil(t, evening)
	assert.Equal(t, morning.Format(time.DateOnly), evening.Format(time.DateOnly))
}

// TestDateForCrossesMonthAndYearBoundaries — AddDate rather than adding a
// duration, so 365 days from December lands in the next year rather than
// overflowing an hour count.
func TestDateForCrossesMonthAndYearBoundaries(t *testing.T) {
	t.Parallel()

	got := expiry.DateFor(time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC), expiry.Resolution{Days: days(365)})

	require.NotNil(t, got)
	assert.Equal(t, "2027-12-20", got.Format(time.DateOnly))
}

// TestDateForHandlesALeapYear keeps the arithmetic honest across 29 February.
func TestDateForHandlesALeapYear(t *testing.T) {
	t.Parallel()

	got := expiry.DateFor(time.Date(2028, 2, 27, 0, 0, 0, 0, time.UTC), expiry.Resolution{Days: days(3)})

	require.NotNil(t, got)
	assert.Equal(t, "2028-03-01", got.Format(time.DateOnly), "2028 is a leap year, so 27 Feb + 3 is 1 March")
}

// TestDateForNoExpirationIsNil — "no expiration" is a real answer, and it must
// not become a zero date.
func TestDateForNoExpirationIsNil(t *testing.T) {
	t.Parallel()

	assert.Nil(t, expiry.DateFor(time.Now(), expiry.Resolution{Days: nil}))
}
