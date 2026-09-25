package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// ntfySettings is a minimal valid configuration, enabled, at 08:00.
func ntfySettings() store.NotificationSettingsInput {
	token := "tk_super_secret"
	return store.NotificationSettingsInput{
		Enabled: true, Kind: store.NotificationNtfy,
		URL: "https://ntfy.example/inventory", Token: &token, SendHour: 8,
	}
}

func TestSaveNotificationSettingsUpsertsOneRowPerStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	first, err := s.SaveNotificationSettings(ctx, storageID, ntfySettings())
	require.NoError(t, err)
	assert.True(t, first.Enabled)
	assert.Equal(t, 8, first.SendHour)

	changed := ntfySettings()
	changed.SendHour = 19
	changed.IncludeSoon = true
	second, err := s.SaveNotificationSettings(ctx, storageID, changed)
	require.NoError(t, err)
	assert.Equal(t, 19, second.SendHour)
	assert.True(t, second.IncludeSoon)

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM notification_settings WHERE storage_id = $1`, storageID),
		"storage_id is the primary key: a storage cannot hold two configurations")
}

// A nil token means "the caller said nothing about it". The value is never
// read back, so a save that does not mention it must not erase it; an empty
// string is the deliberate "remove it".
func TestSaveNotificationSettingsTokenIsThreeStated(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	_, err := s.SaveNotificationSettings(ctx, storageID, ntfySettings())
	require.NoError(t, err)

	keep := ntfySettings()
	keep.Token = nil
	kept, err := s.SaveNotificationSettings(ctx, storageID, keep)
	require.NoError(t, err)
	assert.Equal(t, "tk_super_secret", kept.Token, "an unmentioned token survives")

	clear := ntfySettings()
	cleared := ""
	clear.Token = &cleared
	after, err := s.SaveNotificationSettings(ctx, storageID, clear)
	require.NoError(t, err)
	assert.Empty(t, after.Token)
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM notification_settings WHERE storage_id = $1 AND token IS NULL`, storageID),
		"an empty token is stored as NULL, not as an empty string")
}

func TestSaveNotificationSettingsRejectsNonsense(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	for _, tc := range []struct {
		name string
		in   store.NotificationSettingsInput
	}{
		{"an hour outside the day", func() store.NotificationSettingsInput {
			in := ntfySettings()
			in.SendHour = 24
			return in
		}()},
		{"a negative hour", func() store.NotificationSettingsInput {
			in := ntfySettings()
			in.SendHour = -1
			return in
		}()},
		{"an unknown kind", func() store.NotificationSettingsInput {
			in := ntfySettings()
			in.Kind = "telegram"
			return in
		}()},
		{"no url", func() store.NotificationSettingsInput {
			in := ntfySettings()
			in.URL = ""
			return in
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SaveNotificationSettings(ctx, storageID, tc.in)
			require.ErrorIs(t, err, store.ErrValidation)
		})
	}
}

func TestNotificationSettingsForIsNotFoundWhenNeverConfigured(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, err := s.NotificationSettingsFor(ctx, newStorage(t, ctx))
	require.ErrorIs(t, err, store.ErrNotFound)
}

// The once-a-day guarantee, claimed rather than recorded: the second call in
// the same day returns nothing even though the row is still enabled and the
// hour still matches.
func TestClaimDueNotificationsClaimsOncePerDay(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	_, err := s.SaveNotificationSettings(ctx, storageID, ntfySettings())
	require.NoError(t, err)

	now := time.Date(2026, time.September, 21, 8, 5, 0, 0, time.UTC)
	dayStart := time.Date(2026, time.September, 21, 0, 0, 0, 0, time.UTC)

	// The package shares one database, so other tests' storages may be due in
	// the same hour. Every assertion is about this storage's own row.
	first, err := s.ClaimDueNotifications(ctx, now, dayStart)
	require.NoError(t, err)
	claimed := findClaim(first, storageID)
	require.NotNil(t, claimed)
	assert.Equal(t, "tk_super_secret", claimed.Token, "delivery needs the token; no read of the API returns it")

	again, err := s.ClaimDueNotifications(ctx, now.Add(time.Minute), dayStart)
	require.NoError(t, err)
	assert.Nil(t, findClaim(again, storageID), "a restart within the same hour must not repeat the digest")

	tomorrow := now.AddDate(0, 0, 1)
	next, err := s.ClaimDueNotifications(ctx, tomorrow, dayStart.AddDate(0, 0, 1))
	require.NoError(t, err)
	assert.NotNil(t, findClaim(next, storageID), "the next day's run is the retry")
}

// findClaim picks one storage's row out of a claim, or nil.
func findClaim(claimed []store.NotificationSettings, storageID uuid.UUID) *store.NotificationSettings {
	for i, settings := range claimed {
		if settings.StorageID == storageID {
			return &claimed[i]
		}
	}
	return nil
}

// "With no notification_settings row, or enabled = FALSE, the scheduler does
// nothing for that storage" — the claim is the only way a storage reaches
// delivery, so this is where that has to hold.
func TestClaimDueNotificationsSkipsDisabledAndOtherHours(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	dayStart := time.Date(2026, time.September, 21, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, time.September, 21, 8, 0, 0, 0, time.UTC)

	disabled := newStorage(t, ctx)
	off := ntfySettings()
	off.Enabled = false
	_, err := s.SaveNotificationSettings(ctx, disabled, off)
	require.NoError(t, err)

	otherHour := newStorage(t, ctx)
	later := ntfySettings()
	later.SendHour = 20
	_, err = s.SaveNotificationSettings(ctx, otherHour, later)
	require.NoError(t, err)

	// A storage with no row at all.
	newStorage(t, ctx)

	claimed, err := s.ClaimDueNotifications(ctx, now, dayStart)
	require.NoError(t, err)
	for _, settings := range claimed {
		assert.NotEqual(t, disabled, settings.StorageID)
		assert.NotEqual(t, otherHour, settings.StorageID)
	}

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM notification_settings WHERE storage_id = $1 AND last_run_at IS NOT NULL`, disabled),
		"a disabled row is not even claimed")
}

// Deleting the storage stops delivery immediately, by schema rather than by
// anybody remembering to clean up.
func TestDeletingAStorageRemovesItsNotificationSettings(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	_, err := s.SaveNotificationSettings(ctx, storageID, ntfySettings())
	require.NoError(t, err)

	_, err = execTest(ctx, `DELETE FROM storages WHERE id = $1`, storageID)
	require.NoError(t, err)

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM notification_settings WHERE storage_id = $1`, storageID))
}

func TestRecordNotificationResultWritesTheOutcome(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	_, err := s.SaveNotificationSettings(ctx, storageID, ntfySettings())
	require.NoError(t, err)

	require.NoError(t, s.RecordNotificationResult(ctx, storageID, store.NotificationSent))

	settings, err := s.NotificationSettingsFor(ctx, storageID)
	require.NoError(t, err)
	require.NotNil(t, settings.LastResult)
	assert.Equal(t, store.NotificationSent, *settings.LastResult)
}

// The digest's candidate query: this storage only, live stock only, with the
// location path already resolved. Bucketing is expiry.Classify's job, so this
// checks what is fetched rather than how it is grouped.
func TestExpiringBatchesBeforeIsScopedAndResolvesPaths(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	otherStorage := newStorage(t, ctx)

	basement, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Basement"})
	require.NoError(t, err)
	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Right Shelf", ParentID: &basement.ID})
	require.NoError(t, err)

	milk, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)
	gone, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Finished Yogurt"})
	require.NoError(t, err)
	far, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Flour"})
	require.NoError(t, err)

	insertBatch(t, ctx, milk.ID, shelf.ID, 2, day(2026, time.September, 19))
	insertBatch(t, ctx, gone.ID, shelf.ID, 0, day(2026, time.September, 20))
	insertBatch(t, ctx, far.ID, shelf.ID, 1, day(2027, time.January, 1))

	// A neighbour's storage, expiring on the same day.
	otherLocation, err := s.CreateLocation(ctx, otherStorage, store.NewLocation{Name: "Their Fridge"})
	require.NoError(t, err)
	theirs, err := s.CreateProduct(ctx, otherStorage, store.NewProduct{Name: "Their Milk"})
	require.NoError(t, err)
	insertBatch(t, ctx, theirs.ID, otherLocation.ID, 1, day(2026, time.September, 19))

	until := time.Date(2026, time.October, 5, 0, 0, 0, 0, time.UTC)
	items, err := s.ExpiringBatchesBefore(ctx, storageID, until)
	require.NoError(t, err)

	require.Len(t, items, 1, "a consumed-to-zero batch and a date past the window are both out")
	assert.Equal(t, "Milk", items[0].ProductName)
	assert.Equal(t, 2, items[0].Quantity)
	assert.Equal(t, "Basement > Right Shelf", items[0].LocationPath)
	assert.Equal(t, "2026-09-19", items[0].ExpirationDate.Format(time.DateOnly))
}

func insertBatch(t *testing.T, ctx context.Context, productID, locationID uuid.UUID, quantity int, expires *time.Time) {
	t.Helper()

	_, err := execTest(ctx, `
		INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_date, expiration_source)
		VALUES ($1, $2, $3, $4, $5, 'derived')`,
		newUUID(t), productID, locationID, quantity, expires)
	require.NoError(t, err)
}
