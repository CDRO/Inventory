package httpapi

import (
	"encoding/json"
	"net/http"
	"time"
)

type questResponse struct {
	Generator   string          `json:"generator"`
	Params      json.RawMessage `json:"params"`
	TargetCount int             `json:"target_count"`
	Progress    int             `json:"progress"`
	XPReward    int             `json:"xp_reward"`
	CompletedAt *string         `json:"completed_at"`
}

type weeklyGoalResponse struct {
	Target  int `json:"target"`
	Current int `json:"current"`
}

type questsResponse struct {
	WeekStart       string             `json:"week_start"`
	Quests          []questResponse    `json:"quests"`
	AllClear        bool               `json:"all_clear"`
	CleanStreakDays int                `json:"clean_streak_days"`
	IsComeback      bool               `json:"is_comeback"`
	WeeklyGoal      weeklyGoalResponse `json:"weekly_goal"`
}

// Quests serves GET /api/storages/{storage_id}/quests: this week's quests
// with live progress, or the all-clear read when there are none
// (docs/specs/52-gamification-quests-and-ui.md). Answers 204 with no body
// when the caller has gamification off — the same gate Progress uses, so a
// disabled caller never sees quest data even if the frontend asked anyway.
func (h *GamificationHandler) Quests(w http.ResponseWriter, r *http.Request) {
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

	snap, err := h.store.QuestsForStorage(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	quests := make([]questResponse, len(snap.Quests))
	for i, q := range snap.Quests {
		quests[i] = questResponse{
			Generator:   string(q.Generator),
			Params:      q.Params,
			TargetCount: q.TargetCount,
			Progress:    q.Progress,
			XPReward:    q.XPReward,
			CompletedAt: timestampOrNil(q.CompletedAt),
		}
	}

	writeJSON(w, http.StatusOK, questsResponse{
		WeekStart:       snap.WeekStart.Format(time.DateOnly),
		Quests:          quests,
		AllClear:        len(quests) == 0,
		CleanStreakDays: snap.CleanStreakDays,
		IsComeback:      snap.IsComeback,
		WeeklyGoal:      weeklyGoalResponse{Target: snap.WeeklyGoalTarget, Current: snap.WeeklyGoalCurrent},
	})
}

type achievementResponse struct {
	Key        string `json:"key"`
	UnlockedAt string `json:"unlocked_at"`
}

// Achievements serves GET /api/storages/{storage_id}/achievements: every
// achievement the caller has unlocked in this storage
// (docs/specs/52-gamification-quests-and-ui.md). Hidden achievements
// (immaculate_*) are absent from this list only until unlocked — once
// unlocked they are exactly as visible as any other, matching "announced in
// the dashboard card and a single toast on next visit".
func (h *GamificationHandler) Achievements(w http.ResponseWriter, r *http.Request) {
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

	achievements, err := h.store.AchievementsForUser(r.Context(), storageID, user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	out := make([]achievementResponse, len(achievements))
	for i, a := range achievements {
		out[i] = achievementResponse{Key: a.Key, UnlockedAt: a.UnlockedAt.Format(time.RFC3339)}
	}
	writeJSON(w, http.StatusOK, collection[achievementResponse]{Items: out})
}

func timestampOrNil(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}
