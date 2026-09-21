package expiry_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/CDRO/Inventory/internal/expiry"
)

func date(year int, month time.Month, d int) *time.Time {
	t := time.Date(year, month, d, 0, 0, 0, 0, time.UTC)
	return &t
}

// TestClassifyMatchesTheSpecTable walks the urgency table of
// docs/specs/08-expiration-and-classification.md boundary by boundary. The
// bands are half-open, so each edge belongs to exactly one of them — the
// distinction the digest of docs/specs/17-expiry-notifications.md and the
// dashboard have to agree on.
func TestClassifyMatchesTheSpecTable(t *testing.T) {
	t.Parallel()

	today := time.Date(2026, time.September, 21, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		date *time.Time
		want expiry.Urgency
	}{
		{"yesterday is expired", date(2026, time.September, 20), expiry.UrgencyExpired},
		{"today is critical, not expired", date(2026, time.September, 21), expiry.UrgencyCritical},
		{"two days out is still critical", date(2026, time.September, 23), expiry.UrgencyCritical},
		{"exactly three days out is soon", date(2026, time.September, 24), expiry.UrgencySoon},
		{"thirteen days out is soon", date(2026, time.October, 4), expiry.UrgencySoon},
		{"exactly fourteen days out is ok", date(2026, time.October, 5), expiry.UrgencyOK},
		{"no date at all is none", nil, expiry.UrgencyNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, expiry.Classify(tc.date, today))
		})
	}
}

// TestClassifyIgnoresTheTimeOfDay — both sides are calendar days. A tick at
// 08:05 must classify a batch expiring "today" exactly as a tick at midnight
// would, or the same batch changes bucket over the course of a day.
func TestClassifyIgnoresTheTimeOfDay(t *testing.T) {
	t.Parallel()

	expires := date(2026, time.September, 21)
	for _, hour := range []int{0, 8, 13, 23} {
		today := time.Date(2026, time.September, 21, hour, 59, 59, 0, time.UTC)
		assert.Equal(t, expiry.UrgencyCritical, expiry.Classify(expires, today), "at %02d:59", hour)
	}
}

// TestClassifyComparesCalendarDaysAcrossZones is the bug this is shaped to
// prevent: an expiration DATE arrives from the database as midnight UTC while
// "today" comes from the server's own zone. Compared as instants, a server
// west of UTC would call a batch expiring today already expired, and one east
// of UTC would call yesterday's batch critical.
func TestClassifyComparesCalendarDaysAcrossZones(t *testing.T) {
	t.Parallel()

	west := time.FixedZone("UTC-5", -5*60*60)
	east := time.FixedZone("UTC+11", 11*60*60)

	assert.Equal(t, expiry.UrgencyCritical,
		expiry.Classify(date(2026, time.September, 21),
			time.Date(2026, time.September, 21, 0, 30, 0, 0, west)),
		"expiring today is not expired, whatever the offset")

	assert.Equal(t, expiry.UrgencyExpired,
		expiry.Classify(date(2026, time.September, 20),
			time.Date(2026, time.September, 21, 23, 30, 0, 0, east)),
		"yesterday is expired, whatever the offset")
}
