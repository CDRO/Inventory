package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// GamificationStore is the slice of the store the gamification routes use.
type GamificationStore interface {
	UserPreferencesFor(ctx context.Context, userID uuid.UUID) (*store.UserPreferences, error)
	SetGamificationEnabled(ctx context.Context, userID uuid.UUID, enabled bool) error
	SetHolidayWeeks(ctx context.Context, userID uuid.UUID, weeks []time.Time) error

	UserProgressInStorage(ctx context.Context, storageID, userID uuid.UUID) (*store.UserProgress, error)
	HealthScoreForStorage(ctx context.Context, storageID uuid.UUID) (float64, error)
	Leaderboard(ctx context.Context, storageID uuid.UUID) ([]store.LeaderboardEntry, error)
	StorageGamificationSettingsFor(ctx context.Context, storageID uuid.UUID) (*store.StorageGamificationSettings, error)
	UpdateStorageGamificationSettings(ctx context.Context, storageID uuid.UUID, leaderboardEnabled bool, weeklyGoalItems int) error

	OverallProgressForUser(ctx context.Context, userID uuid.UUID) (*store.OverallProgress, error)

	QuestsForStorage(ctx context.Context, storageID uuid.UUID) (*store.QuestsSnapshot, error)
	AchievementsForUser(ctx context.Context, storageID, userID uuid.UUID) ([]store.UnlockedAchievement, error)
}

// GamificationHandler serves docs/specs/51-gamification-scoring.md's API
// surface.
type GamificationHandler struct {
	store  GamificationStore
	errors *ErrorWriter
}

// NewGamificationHandler wires the handlers to a store and the one error
// writer.
func NewGamificationHandler(s GamificationStore, errs *ErrorWriter) *GamificationHandler {
	return &GamificationHandler{store: s, errors: errs}
}

// enabledFor reports whether the caller has gamification on, writing the
// error response itself on failure so every handler below can just `return`
// on false.
func (h *GamificationHandler) enabledFor(w http.ResponseWriter, r *http.Request, userID uuid.UUID) (enabled bool, ok bool) {
	prefs, err := h.store.UserPreferencesFor(r.Context(), userID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return false, false
	}
	return prefs.GamificationEnabled, true
}

type progressResponse struct {
	XP          int `json:"xp"`
	Level       int `json:"level"`
	StreakWeeks int `json:"streak_weeks"`
	// XPForLevel and XPForNextLevel are the current and next level's
	// thresholds, computed server-side (docs/specs/52-gamification-quests-and-ui.md's
	// header-ring popover) so the frontend never re-implements the level
	// curve just to answer "how much more?".
	XPForLevel     int     `json:"xp_for_level"`
	XPForNextLevel int     `json:"xp_for_next_level"`
	LastActiveWeek *string `json:"last_active_week"`
	HealthScore    float64 `json:"health_score"`
}

// Progress serves GET /api/storages/{storage_id}/progress: the caller's XP,
// level and streak in this storage, plus the storage's health score.
//
// gamification_enabled = FALSE answers 204 with no body, per
// docs/specs/51-gamification-scoring.md — not a placeholder object the
// frontend would have to know to hide.
func (h *GamificationHandler) Progress(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	enabled, ok := h.enabledFor(w, r, user.ID)
	if !ok {
		return
	}
	if !enabled {
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	progress, err := h.store.UserProgressInStorage(r.Context(), storageID, user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	health, err := h.store.HealthScoreForStorage(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusOK, progressResponse{
		XP: progress.XP, Level: progress.Level, StreakWeeks: progress.StreakWeeks,
		XPForLevel:     store.XPThresholdForLevel(progress.Level),
		XPForNextLevel: store.XPThresholdForLevel(progress.Level + 1),
		LastActiveWeek: dateOrNil(progress.LastActiveWeek), HealthScore: health,
	})
}

type leaderboardEntryResponse struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	XP          int       `json:"xp"`
	Level       int       `json:"level"`
}

// Leaderboard serves GET /api/storages/{storage_id}/progress/leaderboard.
//
// 404, not an empty list, when the storage has not turned it on
// (docs/specs/51-gamification-scoring.md) — a storage that never enabled
// individual ranking should look exactly like one this route does not exist
// for, matching the same "unknown and inaccessible look identical" discipline
// the storage-membership gate itself uses.
func (h *GamificationHandler) Leaderboard(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	enabled, ok := h.enabledFor(w, r, user.ID)
	if !ok {
		return
	}
	if !enabled {
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	settings, err := h.store.StorageGamificationSettingsFor(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	if !settings.LeaderboardEnabled {
		h.errors.WriteError(w, r, NotFound("leaderboard not enabled for this storage"))
		return
	}

	entries, err := h.store.Leaderboard(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	out := make([]leaderboardEntryResponse, len(entries))
	for i, e := range entries {
		out[i] = leaderboardEntryResponse{UserID: e.UserID, DisplayName: e.DisplayName, XP: e.XP, Level: e.Level}
	}
	writeJSON(w, http.StatusOK, collection[leaderboardEntryResponse]{Items: out})
}

type storageSettingsResponse struct {
	LeaderboardEnabled bool `json:"leaderboard_enabled"`
	WeeklyGoalItems    int  `json:"weekly_goal_items"`
}

// StorageSettings serves GET /api/storages/{storage_id}/gamification/settings.
func (h *GamificationHandler) StorageSettings(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	settings, err := h.store.StorageGamificationSettingsFor(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	writeJSON(w, http.StatusOK, storageSettingsResponse{
		LeaderboardEnabled: settings.LeaderboardEnabled, WeeklyGoalItems: settings.WeeklyGoalItems,
	})
}

// UpdateStorageSettings serves PUT /api/storages/{storage_id}/gamification/settings.
//
// Any member may change these: rights inside a storage are flat, with no
// role column (docs/specs/03-auth-and-multi-tenancy.md), and RequireStorageMember
// already gates this route the same as every other one under
// /api/storages/{storage_id}.
func (h *GamificationHandler) UpdateStorageSettings(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	var body struct {
		LeaderboardEnabled bool `json:"leaderboard_enabled"`
		WeeklyGoalItems    int  `json:"weekly_goal_items"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if err := h.store.UpdateStorageGamificationSettings(r.Context(), storageID, body.LeaderboardEnabled, body.WeeklyGoalItems); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "storage not found"))
		return
	}
	writeJSON(w, http.StatusOK, storageSettingsResponse{
		LeaderboardEnabled: body.LeaderboardEnabled, WeeklyGoalItems: body.WeeklyGoalItems,
	})
}

type storageProgressResponse struct {
	StorageID   uuid.UUID `json:"storage_id"`
	Name        string    `json:"name"`
	XP          int       `json:"xp"`
	Level       int       `json:"level"`
	StreakWeeks int       `json:"streak_weeks"`
}

type overallProgressResponse struct {
	TotalXP            int                       `json:"total_xp"`
	OverallLevel       int                       `json:"overall_level"`
	LongestStreakWeeks int                       `json:"longest_streak_weeks"`
	PerStorage         []storageProgressResponse `json:"per_storage"`
}

// MeProgress serves GET /api/me/progress: the caller's progress aggregated
// over every storage they belong to (docs/specs/51-gamification-scoring.md).
// It lists only storages StoragesForUser already would — the same set
// GET /api/auth/me exposes — so this never reveals a storage the caller
// lacks access to.
func (h *GamificationHandler) MeProgress(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	enabled, ok := h.enabledFor(w, r, user.ID)
	if !ok {
		return
	}
	if !enabled {
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	overall, err := h.store.OverallProgressForUser(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	perStorage := make([]storageProgressResponse, len(overall.PerStorage))
	for i, p := range overall.PerStorage {
		perStorage[i] = storageProgressResponse{
			StorageID: p.StorageID, Name: p.Name, XP: p.XP, Level: p.Level, StreakWeeks: p.StreakWeeks,
		}
	}
	writeJSON(w, http.StatusOK, overallProgressResponse{
		TotalXP: overall.TotalXP, OverallLevel: overall.OverallLevel,
		LongestStreakWeeks: overall.LongestStreakWeeks, PerStorage: perStorage,
	})
}

type preferencesResponse struct {
	GamificationEnabled bool     `json:"gamification_enabled"`
	HolidayWeeks        []string `json:"holiday_weeks"`
}

// MePreferences serves GET /api/me/preferences: the caller's own opt-out and
// holiday weeks. Unlike the progress routes above, this always answers —
// reading your own setting must work even while the layer is off, or there
// would be no way to turn it back on.
func (h *GamificationHandler) MePreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	prefs, err := h.store.UserPreferencesFor(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	writeJSON(w, http.StatusOK, newPreferencesResponse(prefs))
}

// UpdateMePreferences serves PUT /api/me/preferences.
//
// holiday_weeks replaces the whole set — the body is the new truth, not a
// patch — so a client that wants to add one week sends its current list plus
// the addition.
func (h *GamificationHandler) UpdateMePreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	var body struct {
		GamificationEnabled bool     `json:"gamification_enabled"`
		HolidayWeeks        []string `json:"holiday_weeks"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	weeks := make([]time.Time, 0, len(body.HolidayWeeks))
	for i, raw := range body.HolidayWeeks {
		week, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			fields["holiday_weeks"] = append(fields["holiday_weeks"],
				"Entry "+strconv.Itoa(i)+" must be a YYYY-MM-DD date.")
			continue
		}
		weeks = append(weeks, week)
	}
	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	if err := h.store.SetGamificationEnabled(r.Context(), user.ID, body.GamificationEnabled); err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	// SetHolidayWeeks enforces docs/specs/52-gamification-quests-and-ui.md's
	// holiday-mode rules: ErrValidation for a past week, ErrConflict — with a
	// message naming how many weeks remain — for exceeding the rolling
	// 52-week budget.
	if err := h.store.SetHolidayWeeks(r.Context(), user.ID, weeks); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "holiday week rejected"))
		return
	}

	prefs, err := h.store.UserPreferencesFor(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	writeJSON(w, http.StatusOK, newPreferencesResponse(prefs))
}

func newPreferencesResponse(prefs *store.UserPreferences) preferencesResponse {
	weeks := make([]string, len(prefs.HolidayWeeks))
	for i, w := range prefs.HolidayWeeks {
		weeks[i] = w.Format(time.DateOnly)
	}
	return preferencesResponse{GamificationEnabled: prefs.GamificationEnabled, HolidayWeeks: weeks}
}

func dateOrNil(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}
