package notify_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/notify"
	"github.com/CDRO/Inventory/internal/store"
)

func day(year int, month time.Month, d int) time.Time {
	return time.Date(year, month, d, 0, 0, 0, 0, time.UTC)
}

func item(name string, quantity int, location string, date time.Time) store.DigestItem {
	return store.DigestItem{ProductName: name, Quantity: quantity, LocationPath: location, ExpirationDate: date}
}

// The buckets are the urgency bands of spec 08, and the boundaries are the
// half-open ones that spec states: exactly three days out is soon, exactly
// fourteen is neither.
func TestBuildBucketsByUrgency(t *testing.T) {
	today := day(2026, time.September, 21)
	items := []store.DigestItem{
		item("Milk", 2, "Fridge > Door", day(2026, time.September, 19)),
		item("Yogurt", 1, "Fridge > Top", today),
		item("Cheese", 1, "Fridge > Drawer", day(2026, time.September, 23)),
		item("Butter", 1, "Fridge > Door", day(2026, time.September, 24)),
		item("Flour", 1, "Pantry", day(2026, time.October, 5)),
	}

	digest := notify.Build(items, today, true)

	require.Len(t, digest.Expired, 1, "only the date before today")
	require.Equal(t, "Milk", digest.Expired[0].ProductName)
	require.Len(t, digest.Critical, 2, "today and two days out")
	require.Len(t, digest.Soon, 1, "exactly three days out is soon")
	require.Equal(t, "Butter", digest.Soon[0].ProductName)
	require.NotContains(t, digest.Soon, items[4],
		"exactly fourteen days out is ok, and ok is in no bucket at all")
}

// include_soon decides what is reported, and an unreported bucket must not
// keep an otherwise-empty digest alive: a storage with nothing but
// soon-expiring items and the setting off is not notified at all.
func TestBuildHonoursIncludeSoon(t *testing.T) {
	today := day(2026, time.September, 21)
	items := []store.DigestItem{item("Flour", 1, "Pantry", day(2026, time.September, 30))}

	require.False(t, notify.Build(items, today, true).Empty())

	digest := notify.Build(items, today, false)
	require.Empty(t, digest.Soon)
	require.True(t, digest.Empty(), "nothing is sent, and the run records 'empty'")
}

func TestMessageCarriesTheSpecifiedFieldsAndNothingElse(t *testing.T) {
	today := day(2026, time.September, 21)
	digest := notify.Build([]store.DigestItem{
		item("Milk", 2, "Fridge > Door", day(2026, time.September, 19)),
		item("Yogurt", 1, "Fridge > Top", day(2026, time.September, 22)),
	}, today, false)

	message := digest.Message()
	require.Contains(t, message, "Milk")
	require.Contains(t, message, "x2")
	require.Contains(t, message, "Fridge > Door")
	require.Contains(t, message, "2026-09-19")

	// No images, ever: a photo taken inside the home must not be pushed to a
	// relay, so no line may reference one.
	for _, forbidden := range []string{"http://", "https://", ".jpg", ".png", "image", "/api/"} {
		require.NotContains(t, strings.ToLower(message), forbidden)
	}
}

func TestTitleSummarisesTheBuckets(t *testing.T) {
	today := day(2026, time.September, 21)
	digest := notify.Build([]store.DigestItem{
		item("Milk", 2, "Fridge", day(2026, time.September, 19)),
		item("Yogurt", 1, "Fridge", day(2026, time.September, 22)),
		item("Cheese", 1, "Fridge", day(2026, time.September, 22)),
	}, today, false)

	title := digest.Title()
	require.Contains(t, title, "1 expired")
	require.Contains(t, title, "2 expiring within 3 days")
}
