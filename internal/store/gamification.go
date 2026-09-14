package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/gamification"
)

// UserProgress is one user's cached XP, level and streak in one storage
// (docs/specs/51-gamification-scoring.md). It is always reconstructible from
// inventory_logs and contribution_events; nothing here is authoritative.
type UserProgress struct {
	StorageID      uuid.UUID
	UserID         uuid.UUID
	XP             int
	Level          int
	StreakWeeks    int
	LastActiveWeek *time.Time
	RecomputedAt   time.Time
}

// StorageProgress is one storage's contribution to a user's overall progress.
type StorageProgress struct {
	StorageID   uuid.UUID
	Name        string
	XP          int
	Level       int
	StreakWeeks int
}

// OverallProgress is a user's progress aggregated across every storage they
// belong to — GET /api/me/progress. TotalXP is the sum over the caller's
// storages, and OverallLevel is derived from that sum with the same formula
// as a per-storage level, so it is never the sum of the per-storage levels.
type OverallProgress struct {
	TotalXP            int
	OverallLevel       int
	LongestStreakWeeks int
	PerStorage         []StorageProgress
}

// LeaderboardEntry is one member's standing in a storage's optional
// leaderboard.
type LeaderboardEntry struct {
	UserID      uuid.UUID
	DisplayName string
	XP          int
	Level       int
}

// UserPreferences is a user's own gamification opt-out and holiday weeks
// (docs/specs/51-gamification-scoring.md). Both are global to the user, not
// scoped to any one storage.
type UserPreferences struct {
	UserID              uuid.UUID
	GamificationEnabled bool
	HolidayWeeks        []time.Time
}

// StorageGamificationSettings is a storage's flat, any-member-may-change
// toggles (docs/specs/51-gamification-scoring.md, docs/specs/03-auth-and-multi-tenancy.md).
type StorageGamificationSettings struct {
	StorageID          uuid.UUID
	LeaderboardEnabled bool
	WeeklyGoalItems    int
}

// RecordContribution appends a contribution_events row and refreshes
// user_progress in the same transaction as the change that earned it
// (docs/specs/51-gamification-scoring.md) — server-written, append-only,
// never client-supplied.
//
// Unexported and tx-scoped like writeLog: the callers are the specific,
// reviewed integration points in ingestion.go, shoppinglists.go, reorder.go,
// expiry.go and locations.go, each already inside its own transaction.
func recordContribution(ctx context.Context, tx pgx.Tx, storageID, userID uuid.UUID, kind gamification.ContributionKind, refID *uuid.UUID) error {
	id, err := newID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO contribution_events (id, storage_id, user_id, kind, ref_id)
		VALUES ($1, $2, $3, $4, $5)`,
		id, storageID, userID, string(kind), refID); err != nil {
		return fmt.Errorf("store: record contribution: %w", err)
	}
	if err := bumpProgress(ctx, tx, storageID, userID, gamification.XPForContribution(kind), time.Now()); err != nil {
		return err
	}
	if err := advanceQuests(ctx, tx, storageID, userID, string(kind)); err != nil {
		return err
	}
	return evaluateContributionAchievements(ctx, tx, storageID, userID, kind, refID)
}

// bumpForLedgerReason is the live-path counterpart to recordContribution for
// the three ledger reasons that score (docs/specs/51-gamification-scoring.md):
// 'purchase', 'consumption' and 'vision_ingestion'. It is a no-op for every
// other reason and for a nil userID — a system-attributed change earns
// nobody XP.
//
// This intentionally does not apply the 2-hour coalescing window or the
// vision_ingestion/ai_correction supersede rule: those need the full ledger
// history to evaluate correctly, which is exactly what the nightly
// RecomputeAllProgress has and a single write does not. A live bump is
// therefore optimistic — it assumes this contribution is new — and may be
// revised slightly downward at the next recompute, never upward and never
// framed as a loss (docs/specs/51-gamification-scoring.md).
func bumpForLedgerReason(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, userID *uuid.UUID, reason LogReason) error {
	if userID == nil {
		return nil
	}
	xp, ok := gamification.XPForLedgerReason(string(reason))
	if !ok {
		return nil
	}
	if err := bumpProgress(ctx, tx, storageID, *userID, xp, time.Now()); err != nil {
		return err
	}
	if err := advanceQuests(ctx, tx, storageID, *userID, string(reason)); err != nil {
		return err
	}
	return evaluateLedgerAchievements(ctx, tx, storageID, *userID, reason)
}

// bumpProgress increments a user's cached XP and recomputes their level and
// last-active week, creating the row if this is their first scored activity
// in this storage.
func bumpProgress(ctx context.Context, tx pgx.Tx, storageID, userID uuid.UUID, xpDelta int, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_progress (storage_id, user_id) VALUES ($1, $2)
		ON CONFLICT (storage_id, user_id) DO NOTHING`, storageID, userID); err != nil {
		return fmt.Errorf("store: ensure progress row: %w", err)
	}

	var xp int
	if err := tx.QueryRow(ctx, `
		SELECT xp FROM user_progress WHERE storage_id = $1 AND user_id = $2 FOR UPDATE`,
		storageID, userID).Scan(&xp); err != nil {
		return fmt.Errorf("store: lock progress row: %w", err)
	}

	newXP := xp + xpDelta
	week := mondayOf(at)
	if _, err := tx.Exec(ctx, `
		UPDATE user_progress
		   SET xp = $1, level = $2, last_active_week = $3, recomputed_at = now()
		 WHERE storage_id = $4 AND user_id = $5`,
		newXP, gamification.Level(newXP), week, storageID, userID); err != nil {
		return fmt.Errorf("store: update progress: %w", err)
	}
	return nil
}

// mondayOf returns the Monday of t's week, per user_progress.last_active_week
// and holiday_weeks.week_start (docs/specs/51-gamification-scoring.md).
func mondayOf(t time.Time) time.Time {
	t = t.UTC()
	// time.Weekday is 0 for Sunday; shifting by 6 first makes Monday the zero
	// point, so the subtraction below always lands on or before t.
	offset := (int(t.Weekday()) + 6) % 7
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -offset)
}

// RecomputeAllProgress rebuilds user_progress from inventory_logs and
// contribution_events from scratch, and is what makes the cache "fully
// rebuildable via the /inventory recompute-progress maintenance subcommand"
// (docs/specs/51-gamification-scoring.md) true rather than aspirational. It
// returns how many (storage, user) rows it touched.
//
// A pair with a cached row but *zero* remaining events — every scored
// product it ever touched has since been deleted, or an admin removed its
// only contribution_events row — is included too, and reset to zero. Without
// this, such a pair simply has no key in allScoringEvents' result and its
// stale cache would survive every future recompute forever, which is exactly
// the create-delete-cycle loophole "deleting a product removes its
// contribution on the next recompute" exists to close.
//
// Each pair is recomputed in its own transaction: a household's worth of
// pairs is small, and a failure partway through must not roll back progress
// that was already correctly rebuilt for someone else.
func (s *Store) RecomputeAllProgress(ctx context.Context) (int, error) {
	events, err := s.allScoringEvents(ctx)
	if err != nil {
		return 0, err
	}

	stale, err := s.existingProgressPairs(ctx)
	if err != nil {
		return 0, err
	}
	for pair := range stale {
		if _, hasEvents := events[pair]; !hasEvents {
			events[pair] = nil
		}
	}

	n := 0
	for pair, pairEvents := range events {
		if err := s.recomputeOnePair(ctx, pair.storageID, pair.userID, pairEvents); err != nil {
			return n, fmt.Errorf("store: recompute progress for storage %s user %s: %w", pair.storageID, pair.userID, err)
		}
		n++
	}
	return n, nil
}

// existingProgressPairs returns every (storage, user) pair that currently has
// a user_progress row, whether or not it still has any scoreable history.
func (s *Store) existingProgressPairs(ctx context.Context) (map[progressPair]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT storage_id, user_id FROM user_progress`)
	if err != nil {
		return nil, fmt.Errorf("store: load existing progress pairs: %w", err)
	}
	defer rows.Close()

	out := map[progressPair]bool{}
	for rows.Next() {
		var pair progressPair
		if err := rows.Scan(&pair.storageID, &pair.userID); err != nil {
			return nil, fmt.Errorf("store: scan existing progress pair: %w", err)
		}
		out[pair] = true
	}
	return out, rows.Err()
}

type progressPair struct {
	storageID uuid.UUID
	userID    uuid.UUID
}

// allScoringEvents loads every event that can earn XP, across every storage
// and user, grouped by (storage, user).
//
// Deleted products never appear here: inventory_logs.product_id cascades
// with the product it names (docs/specs/02-data-model.md), so a recompute
// naturally sees only current data — see Coalesce's doc comment for why that
// is also what makes "create-delete cycles earn nothing" hold without extra
// bookkeeping.
func (s *Store) allScoringEvents(ctx context.Context) (map[progressPair][]gamification.Event, error) {
	out := map[progressPair][]gamification.Event{}

	ledgerRows, err := s.pool.Query(ctx, `
		SELECT p.storage_id, l.created_by, l.product_id, l.reason, l.timestamp
		  FROM inventory_logs l
		  JOIN products p ON p.id = l.product_id
		 WHERE l.reason IN ('purchase', 'consumption', 'vision_ingestion')
		   AND l.created_by IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: load scoring ledger rows: %w", err)
	}
	for ledgerRows.Next() {
		var storageID, userID, productID uuid.UUID
		var reason string
		var at time.Time
		if err := ledgerRows.Scan(&storageID, &userID, &productID, &reason, &at); err != nil {
			ledgerRows.Close()
			return nil, fmt.Errorf("store: scan scoring ledger row: %w", err)
		}
		xp, ok := gamification.XPForLedgerReason(reason)
		if !ok {
			continue
		}
		pair := progressPair{storageID: storageID, userID: userID}
		out[pair] = append(out[pair], gamification.Event{RefID: productID, Kind: reason, At: at, XP: xp})
	}
	if err := ledgerRows.Err(); err != nil {
		ledgerRows.Close()
		return nil, fmt.Errorf("store: load scoring ledger rows: %w", err)
	}
	ledgerRows.Close()

	contribRows, err := s.pool.Query(ctx, `
		SELECT id, storage_id, user_id, kind, ref_id, created_at FROM contribution_events`)
	if err != nil {
		return nil, fmt.Errorf("store: load contribution events: %w", err)
	}
	for contribRows.Next() {
		var id, storageID, userID uuid.UUID
		var kind string
		var refID *uuid.UUID
		var at time.Time
		if err := contribRows.Scan(&id, &storageID, &userID, &kind, &refID, &at); err != nil {
			contribRows.Close()
			return nil, fmt.Errorf("store: scan contribution event: %w", err)
		}
		// A row with no ref_id coalesces with nothing else and is always
		// scored on its own — falling back to the row's own id keeps the
		// grouping key well-defined without inventing a shared bucket for
		// unrelated events.
		ref := id
		if refID != nil {
			ref = *refID
		}
		pair := progressPair{storageID: storageID, userID: userID}
		out[pair] = append(out[pair], gamification.Event{
			RefID: ref, Kind: kind, At: at, XP: gamification.XPForContribution(gamification.ContributionKind(kind)),
		})
	}
	if err := contribRows.Err(); err != nil {
		contribRows.Close()
		return nil, fmt.Errorf("store: load contribution events: %w", err)
	}
	contribRows.Close()

	return out, nil
}

// recomputeOnePair rebuilds one (storage, user) pair's cached row.
func (s *Store) recomputeOnePair(ctx context.Context, storageID, userID uuid.UUID, events []gamification.Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		holidayWeeks, err := loadHolidayWeeks(ctx, tx, userID)
		if err != nil {
			return err
		}

		xp := gamification.Coalesce(events)
		activeWeeks := map[time.Time]bool{}
		var lastActive *time.Time
		for _, e := range events {
			week := mondayOf(e.At)
			activeWeeks[week] = true
			if lastActive == nil || week.After(*lastActive) {
				w := week
				lastActive = &w
			}
		}
		streak := computeStreakWeeks(activeWeeks, holidayWeeks, time.Now())

		if _, err := tx.Exec(ctx, `
			INSERT INTO user_progress (storage_id, user_id, xp, level, streak_weeks, last_active_week, recomputed_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())
			ON CONFLICT (storage_id, user_id) DO UPDATE
			   SET xp = EXCLUDED.xp, level = EXCLUDED.level, streak_weeks = EXCLUDED.streak_weeks,
			       last_active_week = EXCLUDED.last_active_week, recomputed_at = now()`,
			storageID, userID, xp, gamification.Level(xp), streak, lastActive); err != nil {
			return fmt.Errorf("store: write recomputed progress: %w", err)
		}

		for _, key := range gamification.SteadyHandTiersReached(streak) {
			if err := unlockAchievement(ctx, tx, storageID, userID, string(key)); err != nil {
				return err
			}
		}
		return nil
	})
}

// computeStreakWeeks counts consecutive weeks, walking backward from now,
// that either had scored activity or were marked holiday — pausing rather
// than breaking the streak, per docs/specs/50-gamification-overview.md
// principle 5. The current week is exempt from breaking the streak on its
// own: it may simply not have happened yet.
//
// The detailed streak *budget* — how holiday weeks are earned, spent, and
// surfaced — is docs/specs/52-gamification-quests-and-ui.md's job; this is
// only the arithmetic the user_progress.streak_weeks column needs to mean
// something as soon as it exists.
func computeStreakWeeks(activeWeeks, holidayWeeks map[time.Time]bool, now time.Time) int {
	week := mondayOf(now)
	streak := 0
	current := true
	for {
		switch {
		case holidayWeeks[week]:
			// Paused: neither counted nor broken — checked before activity so
			// that a week which happens to be both active and on holiday
			// still does not count toward the streak
			// (docs/specs/52-gamification-quests-and-ui.md: "activity during
			// a holiday week ... does not count toward the streak").
		case activeWeeks[week]:
			streak++
		case current:
			// The current week may not have happened yet; that alone must not
			// break a streak earned in prior weeks.
		default:
			return streak
		}
		week = week.AddDate(0, 0, -7)
		current = false
	}
}

func loadHolidayWeeks(ctx context.Context, q querier, userID uuid.UUID) (map[time.Time]bool, error) {
	rows, err := q.Query(ctx, `SELECT week_start FROM holiday_weeks WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: load holiday weeks: %w", err)
	}
	defer rows.Close()

	out := map[time.Time]bool{}
	for rows.Next() {
		var week time.Time
		if err := rows.Scan(&week); err != nil {
			return nil, fmt.Errorf("store: scan holiday week: %w", err)
		}
		out[week.UTC()] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load holiday weeks: %w", err)
	}
	return out, nil
}

// UserProgressInStorage returns the caller's cached progress in one storage.
// A user with no scored activity yet has no row: this reports the zero value
// (xp 0, level 1) rather than an error, since "never contributed" is a valid
// state, not a missing one.
func (s *Store) UserProgressInStorage(ctx context.Context, storageID, userID uuid.UUID) (*UserProgress, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT storage_id, user_id, xp, level, streak_weeks, last_active_week, recomputed_at
		  FROM user_progress WHERE storage_id = $1 AND user_id = $2`, storageID, userID)

	p, err := scanUserProgress(row)
	if errors.Is(err, ErrNotFound) {
		return &UserProgress{StorageID: storageID, UserID: userID, Level: 1, RecomputedAt: time.Now()}, nil
	}
	return p, err
}

// HealthScoreForStorage computes the storage-level inventory health score
// (docs/specs/51-gamification-scoring.md): the mean of five sub-scores,
// computed live rather than cached, since each is a cheap aggregate over the
// storage's own products and batches.
func (s *Store) HealthScoreForStorage(ctx context.Context, storageID uuid.UUID) (float64, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM products WHERE storage_id = $1`, storageID).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count products for health score: %w", err)
	}
	if total == 0 {
		return 0, nil
	}

	pct := func(query string) (float64, error) {
		var n int
		if err := s.pool.QueryRow(ctx, query, storageID).Scan(&n); err != nil {
			return 0, fmt.Errorf("store: health sub-score: %w", err)
		}
		return float64(n) / float64(total) * 100, nil
	}

	categorized, err := pct(`SELECT count(*) FROM products WHERE storage_id = $1 AND category_id IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	imaged, err := pct(`SELECT count(*) FROM products WHERE storage_id = $1 AND (image_url IS NOT NULL OR icon_name IS NOT NULL)`)
	if err != nil {
		return 0, err
	}
	minStockTracked, err := pct(`SELECT count(*) FROM products WHERE storage_id = $1 AND min_stock > 0`)
	if err != nil {
		return 0, err
	}
	recentlyActive, err := pct(`
		SELECT count(DISTINCT p.id) FROM products p
		  JOIN inventory_logs l ON l.product_id = p.id
		 WHERE p.storage_id = $1 AND l.timestamp > now() - interval '180 days'`)
	if err != nil {
		return 0, err
	}

	// The fourth sub-score is over batches, not products, and only the ones
	// that are supposed to carry an expiry at all — a non_perishable batch
	// with no date is correct, not a gap.
	var expiryEligible, expiryTrackedCount int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE b.expiration_date IS NOT NULL)
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE p.storage_id = $1 AND p.item_type IN ('perishable', 'long_shelf_life')`,
		storageID).Scan(&expiryEligible, &expiryTrackedCount); err != nil {
		return 0, fmt.Errorf("store: health sub-score expiry: %w", err)
	}
	expiryTracked := 0.0
	if expiryEligible > 0 {
		expiryTracked = float64(expiryTrackedCount) / float64(expiryEligible) * 100
	}

	return gamification.HealthScore(categorized, imaged, minStockTracked, expiryTracked, recentlyActive), nil
}

// Leaderboard returns every member's progress in one storage, for the
// optional per-member ranking (docs/specs/51-gamification-scoring.md). The
// caller is responsible for the 404-unless-enabled gate — that is a routing
// decision, not a data one.
func (s *Store) Leaderboard(ctx context.Context, storageID uuid.UUID) ([]LeaderboardEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.user_id, u.display_name, coalesce(p.xp, 0), coalesce(p.level, 1)
		  FROM storage_members m
		  JOIN users u ON u.id = m.user_id
		  LEFT JOIN user_progress p ON p.storage_id = m.storage_id AND p.user_id = m.user_id
		 WHERE m.storage_id = $1
		 ORDER BY coalesce(p.xp, 0) DESC, u.display_name`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: load leaderboard: %w", err)
	}
	defer rows.Close()

	out := []LeaderboardEntry{}
	for rows.Next() {
		var e LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.DisplayName, &e.XP, &e.Level); err != nil {
			return nil, fmt.Errorf("store: scan leaderboard entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OverallProgressForUser aggregates a user's progress across every storage
// they belong to (docs/specs/51-gamification-scoring.md). It lists only
// storages the caller already belongs to — the same set StoragesForUser
// returns for GET /api/auth/me — and never reveals one they lack access to.
func (s *Store) OverallProgressForUser(ctx context.Context, userID uuid.UUID) (*OverallProgress, error) {
	storages, err := s.StoragesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	out := &OverallProgress{PerStorage: []StorageProgress{}}
	for _, storage := range storages {
		p, err := s.UserProgressInStorage(ctx, storage.ID, userID)
		if err != nil {
			return nil, err
		}
		out.TotalXP += p.XP
		if p.StreakWeeks > out.LongestStreakWeeks {
			out.LongestStreakWeeks = p.StreakWeeks
		}
		out.PerStorage = append(out.PerStorage, StorageProgress{
			StorageID: storage.ID, Name: storage.Name, XP: p.XP, Level: p.Level, StreakWeeks: p.StreakWeeks,
		})
	}
	out.OverallLevel = gamification.Level(out.TotalXP)
	return out, nil
}

// UserPreferencesFor returns a user's gamification opt-out and holiday
// weeks. A user who has never changed either gets the documented defaults
// (enabled, no holidays) rather than an error.
func (s *Store) UserPreferencesFor(ctx context.Context, userID uuid.UUID) (*UserPreferences, error) {
	prefs := &UserPreferences{UserID: userID, GamificationEnabled: true, HolidayWeeks: []time.Time{}}

	err := s.pool.QueryRow(ctx, `
		SELECT gamification_enabled FROM user_preferences WHERE user_id = $1`, userID).Scan(&prefs.GamificationEnabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: load user preferences: %w", err)
	}

	weeks, err := loadHolidayWeeks(ctx, s.pool, userID)
	if err != nil {
		return nil, err
	}
	for week := range weeks {
		prefs.HolidayWeeks = append(prefs.HolidayWeeks, week)
	}
	return prefs, nil
}

// SetGamificationEnabled sets a user's own opt-out
// (docs/specs/50-gamification-overview.md principle 4). It never touches any
// inventory feature and never affects other members of a shared storage.
func (s *Store) SetGamificationEnabled(ctx context.Context, userID uuid.UUID, enabled bool) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_preferences (user_id, gamification_enabled) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET gamification_enabled = $2, updated_at = now()`,
		userID, enabled)
	if err != nil {
		return fmt.Errorf("store: set gamification enabled: %w", err)
	}
	return nil
}

// SetHolidayWeeks replaces a user's future holiday weeks with the requested
// set, enforcing docs/specs/52-gamification-quests-and-ui.md's holiday-mode
// rules:
//
//   - Only the current week and future weeks may ever be *added*. A past or
//     current week already on record stays on record regardless of whether
//     the caller's list still names it — the body is the new truth for the
//     future, not for history.
//   - Un-marking a future week is a real removal (it frees the budget);
//     un-marking a past or current week is not offered at all, because there
//     is no way to actually free that budget slot without reopening the
//     abuse the budget exists to prevent (see the package doc note below).
//     Silently keeping it, rather than accepting and ignoring the removal,
//     keeps GET immediately consistent with what was just PUT.
//   - Adding weeks that would push any rolling 52-week window over 8 is
//     rejected wholesale with ErrConflict, naming how many weeks remain in
//     that window — nothing is written on that path.
//
// This project's holiday_weeks rows are therefore append-only for any week
// at or before "now": the literal spec text allows un-marking a past or
// current week "without a refund", which would require tracking budget
// usage independently of which rows still exist. Never allowing that
// removal in the first place reaches the same practical outcome — the
// budget slot stays spent — without a second, shadow ledger.
func (s *Store) SetHolidayWeeks(ctx context.Context, userID uuid.UUID, weeks []time.Time) error {
	requested := map[time.Time]bool{}
	for _, w := range weeks {
		requested[mondayOf(w)] = true
	}

	return s.inTx(ctx, func(tx pgx.Tx) error {
		existing, err := loadHolidayWeeks(ctx, tx, userID)
		if err != nil {
			return err
		}

		current := mondayOf(time.Now())
		for week := range requested {
			// A past week already on record is not a violation to reject —
			// GET /api/me/preferences returns full history, and a client
			// that naively resends its current list while adding or
			// removing a future week (web/static/js/pages/settings.js does
			// exactly this) must not have that harmless echo rejected. Only
			// a *new* attempt to backdate a holiday week is refused.
			if week.Before(current) && !existing[week] {
				return fmt.Errorf("%w: only the current week or a future week may be marked as holiday", ErrValidation)
			}
		}

		// final is the complete set this write would leave in place: every
		// past/current week already on record (never removable here), plus
		// exactly the current/future weeks the caller asked for.
		final := map[time.Time]bool{}
		for week := range existing {
			if !week.After(current) {
				final[week] = true
			}
		}
		for week := range requested {
			final[week] = true
		}

		if newlyExceedsBudget(existing, final) {
			remaining := gamification.HolidayBudgetWeeks - gamification.MaxWeeksInWindow(toSlice(existing))
			if remaining < 0 {
				remaining = 0
			}
			return fmt.Errorf("%w: %d week(s) remaining in this 52-week window", ErrConflict, remaining)
		}

		if _, err := tx.Exec(ctx, `
			DELETE FROM holiday_weeks WHERE user_id = $1 AND week_start > $2`, userID, current); err != nil {
			return fmt.Errorf("store: clear future holiday weeks: %w", err)
		}
		for week := range final {
			if week.After(current) || (week.Equal(current) && !existing[week]) {
				if _, err := tx.Exec(ctx, `
					INSERT INTO holiday_weeks (user_id, week_start) VALUES ($1, $2)
					ON CONFLICT DO NOTHING`, userID, week); err != nil {
					return fmt.Errorf("store: insert holiday week: %w", err)
				}
			}
		}
		return nil
	})
}

// newlyExceedsBudget reports whether final introduces at least one week not
// already in existing, AND the resulting set violates the rolling-window
// budget — a request that only removes weeks, or resends exactly what was
// already on record, is never rejected even if legacy data happens to sit
// over the current budget.
func newlyExceedsBudget(existing, final map[time.Time]bool) bool {
	hasNew := false
	for w := range final {
		if !existing[w] {
			hasNew = true
			break
		}
	}
	if !hasNew {
		return false
	}
	return gamification.MaxWeeksInWindow(toSlice(final)) > gamification.HolidayBudgetWeeks
}

func toSlice(weeks map[time.Time]bool) []time.Time {
	out := make([]time.Time, 0, len(weeks))
	for w := range weeks {
		out = append(out, w)
	}
	return out
}

// StorageGamificationSettingsFor returns one storage's toggles, defaulting to
// the documented values (leaderboard off, a goal of 20 weekly items) for a
// storage that has never changed them.
func (s *Store) StorageGamificationSettingsFor(ctx context.Context, storageID uuid.UUID) (*StorageGamificationSettings, error) {
	out := &StorageGamificationSettings{StorageID: storageID, LeaderboardEnabled: false, WeeklyGoalItems: 20}
	err := s.pool.QueryRow(ctx, `
		SELECT leaderboard_enabled, weekly_goal_items
		  FROM storage_gamification_settings WHERE storage_id = $1`, storageID).
		Scan(&out.LeaderboardEnabled, &out.WeeklyGoalItems)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: load storage gamification settings: %w", err)
	}
	return out, nil
}

// UpdateStorageGamificationSettings sets one storage's toggles. Any member
// may call this: rights inside a storage are flat, with no role column
// (docs/specs/03-auth-and-multi-tenancy.md), and gamification settings are
// no exception.
func (s *Store) UpdateStorageGamificationSettings(ctx context.Context, storageID uuid.UUID, leaderboardEnabled bool, weeklyGoalItems int) error {
	if weeklyGoalItems < 0 {
		return fmt.Errorf("%w: weekly goal cannot be negative", ErrValidation)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO storage_gamification_settings (storage_id, leaderboard_enabled, weekly_goal_items)
		VALUES ($1, $2, $3)
		ON CONFLICT (storage_id) DO UPDATE
		   SET leaderboard_enabled = $2, weekly_goal_items = $3, updated_at = now()`,
		storageID, leaderboardEnabled, weeklyGoalItems)
	if err != nil {
		return fmt.Errorf("store: update storage gamification settings: %w", err)
	}
	return nil
}

// XPThresholdForLevel exposes gamification.XPThresholdForLevel to the
// httpapi package, which may not import internal/gamification directly
// (internal/gamification/boundary_test.go) — the header-ring popover
// (docs/specs/52-gamification-quests-and-ui.md) needs it to answer "how much
// more?" without re-implementing the level curve client-side.
func XPThresholdForLevel(level int) int {
	return gamification.XPThresholdForLevel(level)
}

func scanUserProgress(row rowScanner) (*UserProgress, error) {
	var p UserProgress
	err := row.Scan(&p.StorageID, &p.UserID, &p.XP, &p.Level, &p.StreakWeeks, &p.LastActiveWeek, &p.RecomputedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan user progress: %w", err)
	}
	return &p, nil
}
