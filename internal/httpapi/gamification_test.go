package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// fakeGamification is an in-memory GamificationStore.
type fakeGamification struct {
	prefs             map[uuid.UUID]*store.UserPreferences
	progress          map[uuid.UUID]*store.UserProgress // keyed by userID; storageID ignored, one fake storage per test
	health            float64
	settings          *store.StorageGamificationSettings
	leaderboard       []store.LeaderboardEntry
	overall           *store.OverallProgress
	updateSettingsErr error

	lastSettingsUpdate struct {
		leaderboardEnabled bool
		weeklyGoalItems    int
	}
	lastHolidayWeeks []time.Time
}

func newFakeGamification() *fakeGamification {
	return &fakeGamification{
		prefs:    map[uuid.UUID]*store.UserPreferences{},
		progress: map[uuid.UUID]*store.UserProgress{},
		settings: &store.StorageGamificationSettings{LeaderboardEnabled: false, WeeklyGoalItems: 20},
		overall:  &store.OverallProgress{PerStorage: []store.StorageProgress{}},
	}
}

func (f *fakeGamification) UserPreferencesFor(_ context.Context, userID uuid.UUID) (*store.UserPreferences, error) {
	if p, ok := f.prefs[userID]; ok {
		return p, nil
	}
	return &store.UserPreferences{UserID: userID, GamificationEnabled: true, HolidayWeeks: []time.Time{}}, nil
}

func (f *fakeGamification) SetGamificationEnabled(_ context.Context, userID uuid.UUID, enabled bool) error {
	p, ok := f.prefs[userID]
	if !ok {
		p = &store.UserPreferences{UserID: userID, HolidayWeeks: []time.Time{}}
		f.prefs[userID] = p
	}
	p.GamificationEnabled = enabled
	return nil
}

func (f *fakeGamification) SetHolidayWeeks(_ context.Context, userID uuid.UUID, weeks []time.Time) error {
	f.lastHolidayWeeks = weeks
	p, ok := f.prefs[userID]
	if !ok {
		p = &store.UserPreferences{UserID: userID, GamificationEnabled: true}
		f.prefs[userID] = p
	}
	p.HolidayWeeks = weeks
	return nil
}

func (f *fakeGamification) UserProgressInStorage(_ context.Context, storageID, userID uuid.UUID) (*store.UserProgress, error) {
	if p, ok := f.progress[userID]; ok {
		return p, nil
	}
	return &store.UserProgress{StorageID: storageID, UserID: userID, Level: 1}, nil
}

func (f *fakeGamification) HealthScoreForStorage(context.Context, uuid.UUID) (float64, error) {
	return f.health, nil
}

func (f *fakeGamification) Leaderboard(context.Context, uuid.UUID) ([]store.LeaderboardEntry, error) {
	return f.leaderboard, nil
}

func (f *fakeGamification) StorageGamificationSettingsFor(context.Context, uuid.UUID) (*store.StorageGamificationSettings, error) {
	return f.settings, nil
}

func (f *fakeGamification) UpdateStorageGamificationSettings(_ context.Context, _ uuid.UUID, leaderboardEnabled bool, weeklyGoalItems int) error {
	if f.updateSettingsErr != nil {
		return f.updateSettingsErr
	}
	f.lastSettingsUpdate.leaderboardEnabled = leaderboardEnabled
	f.lastSettingsUpdate.weeklyGoalItems = weeklyGoalItems
	f.settings = &store.StorageGamificationSettings{LeaderboardEnabled: leaderboardEnabled, WeeklyGoalItems: weeklyGoalItems}
	return nil
}

func (f *fakeGamification) OverallProgressForUser(context.Context, uuid.UUID) (*store.OverallProgress, error) {
	return f.overall, nil
}

// TestProgressReports200WithHealthScore checks the happy path returns the
// caller's cached progress alongside the storage's health score.
func TestProgressReports200WithHealthScore(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.gamification.progress[f.user.ID] = &store.UserProgress{XP: 42, Level: 2, StreakWeeks: 3}
	f.gamification.health = 61.5

	rec := f.do(http.MethodGet, f.base()+"/progress", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"xp":42`)
	assert.Contains(t, rec.Body.String(), `"level":2`)
	assert.Contains(t, rec.Body.String(), `"streak_weeks":3`)
	assert.Contains(t, rec.Body.String(), `"health_score":61.5`)
}

// TestProgressIsNoContentWhenDisabled is the acceptance criterion from
// docs/specs/51-gamification-scoring.md verbatim: a caller with
// gamification_enabled = FALSE gets 204 with no body from every progress
// endpoint, not a placeholder object.
func TestProgressIsNoContentWhenDisabled(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	require.NoError(t, f.gamification.SetGamificationEnabled(context.Background(), f.user.ID, false))

	rec := f.do(http.MethodGet, f.base()+"/progress", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}

// TestMeProgressIsNoContentWhenDisabled: the /api/me/progress route obeys the
// same opt-out as the storage-scoped one.
func TestMeProgressIsNoContentWhenDisabled(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	require.NoError(t, f.gamification.SetGamificationEnabled(context.Background(), f.user.ID, false))

	rec := f.do(http.MethodGet, "/api/me/progress", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestLeaderboardIs404WhenNotEnabled is the spec's explicit rule: a storage
// that never turned the leaderboard on answers 404, not an empty list —
// indistinguishable from a route that does not exist, the same discipline
// RequireStorageMember itself uses for an inaccessible storage.
func TestLeaderboardIs404WhenNotEnabled(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.gamification.settings.LeaderboardEnabled = false

	rec := f.do(http.MethodGet, f.base()+"/progress/leaderboard", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestLeaderboardListsMembersWhenEnabled(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.gamification.settings.LeaderboardEnabled = true
	f.gamification.leaderboard = []store.LeaderboardEntry{
		{UserID: f.user.ID, DisplayName: "Ada", XP: 90, Level: 3},
	}

	rec := f.do(http.MethodGet, f.base()+"/progress/leaderboard", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"display_name":"Ada"`)
}

// TestStorageSettingsRoundTrip: PUT changes what a subsequent GET returns,
// and any member may call it — no admin gate, matching the flat rights every
// other storage-scoped route already has (docs/specs/03-auth-and-multi-tenancy.md).
func TestStorageSettingsRoundTrip(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPut, f.base()+"/gamification/settings", `{"leaderboard_enabled":true,"weekly_goal_items":30}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, f.gamification.lastSettingsUpdate.leaderboardEnabled)
	assert.Equal(t, 30, f.gamification.lastSettingsUpdate.weeklyGoalItems)

	rec = f.do(http.MethodGet, f.base()+"/gamification/settings", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"leaderboard_enabled":true`)
	assert.Contains(t, rec.Body.String(), `"weekly_goal_items":30`)
}

// TestMePreferencesAlwaysAnswers: reading and writing your own opt-out must
// work even while the layer is off, or there would be no way to turn it back
// on — unlike the progress routes, this never answers 204.
func TestMePreferencesAlwaysAnswers(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	require.NoError(t, f.gamification.SetGamificationEnabled(context.Background(), f.user.ID, false))

	rec := f.do(http.MethodGet, "/api/me/preferences", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"gamification_enabled":false`)
}

// TestUpdateMePreferencesReplacesHolidayWeeks: PUT is a replace, not a patch —
// the body sent is the whole new set of holiday weeks.
func TestUpdateMePreferencesReplacesHolidayWeeks(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPut, "/api/me/preferences",
		`{"gamification_enabled":true,"holiday_weeks":["2026-01-05","2026-01-12"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, f.gamification.lastHolidayWeeks, 2)
	assert.Equal(t, 2026, f.gamification.lastHolidayWeeks[0].Year())
}

func TestUpdateMePreferencesRejectsAMalformedWeek(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPut, "/api/me/preferences",
		`{"gamification_enabled":true,"holiday_weeks":["not-a-date"]}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestMeProgressAggregatesAcrossStorages: the overall response shape carries
// total_xp, overall_level and the per-storage breakdown, none of which are
// scoped to the request's single storage.
func TestMeProgressAggregatesAcrossStorages(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	other := uuid.New()
	f.gamification.overall = &store.OverallProgress{
		TotalXP: 130, OverallLevel: 3, LongestStreakWeeks: 5,
		PerStorage: []store.StorageProgress{
			{StorageID: f.storageID, Name: "Home", XP: 100, Level: 2, StreakWeeks: 5},
			{StorageID: other, Name: "Cellar", XP: 30, Level: 1, StreakWeeks: 1},
		},
	}

	rec := f.do(http.MethodGet, "/api/me/progress", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"total_xp":130`)
	assert.Contains(t, rec.Body.String(), `"overall_level":3`)
	assert.Contains(t, rec.Body.String(), `"Cellar"`)
}
