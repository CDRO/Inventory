package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/gamification"
)

// evaluateContributionAchievements checks the user-attributed achievements
// plausibly satisfied by one contribution_events kind
// (docs/specs/52-gamification-quests-and-ui.md), evaluated right after that
// contribution is recorded — after the event that could plausibly satisfy
// it, never on a timer polling everything.
func evaluateContributionAchievements(ctx context.Context, tx pgx.Tx, storageID, userID uuid.UUID, kind gamification.ContributionKind, refID *uuid.UUID) error {
	switch kind {
	case gamification.KindAICorrection:
		return checkCurator(ctx, tx, storageID, userID)
	case gamification.KindLocationMapped:
		if refID == nil {
			return nil
		}
		return checkCartographer(ctx, tx, storageID, userID, *refID)
	default:
		return nil
	}
}

// first_shelf — "your first shelf-photo ingestion" — is unlocked directly in
// ConfirmIngestion (internal/store/ingestion.go), the one place that has the
// job's kind in scope, rather than here: this file's achievement checks run
// generically per batch or per contribution, with no view of which job
// produced either.

func checkCurator(ctx context.Context, tx pgx.Tx, storageID, userID uuid.UUID) error {
	var count int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM contribution_events WHERE storage_id = $1 AND user_id = $2 AND kind = $3`,
		storageID, userID, string(gamification.KindAICorrection)).Scan(&count); err != nil {
		return fmt.Errorf("store: count ai_correction events: %w", err)
	}
	if count < gamification.CuratorCorrections {
		return nil
	}
	return unlockAchievement(ctx, tx, storageID, userID, string(gamification.AchCurator))
}

func checkCartographer(ctx context.Context, tx pgx.Tx, storageID, userID, locationID uuid.UUID) error {
	depth, err := locationDepth(ctx, tx, locationID)
	if err != nil {
		return err
	}
	if depth < gamification.CartographerTreeDepth {
		return nil
	}
	return unlockAchievement(ctx, tx, storageID, userID, string(gamification.AchCartographer))
}

// locationDepth returns id's depth in its tree, root = 1 — the same
// ancestor-walk shape as tree.go's wouldCycle, bounded the same way.
func locationDepth(ctx context.Context, q querier, id uuid.UUID) (int, error) {
	var depth int
	err := q.QueryRow(ctx, `
		WITH RECURSIVE ancestors AS (
		    SELECT id, parent_id, 1 AS depth FROM locations WHERE id = $1
		    UNION ALL
		    SELECT l.id, l.parent_id, a.depth + 1
		      FROM locations l
		      JOIN ancestors a ON l.id = a.parent_id
		     WHERE a.depth < 64
		)
		SELECT max(depth) FROM ancestors`, id).Scan(&depth)
	if err != nil {
		return 0, fmt.Errorf("store: compute location depth: %w", err)
	}
	return depth, nil
}

// EvaluateStorageAchievements checks the storage-attributed achievements
// (docs/specs/52-gamification-quests-and-ui.md): facts about the storage's
// data as a whole, with no single acting user, paid to every current member
// when crossed — the same "household achievement" pattern the clean-storage
// milestones use, and for the same reason: nobody should be individually
// credited or blamed for a shared inventory's state.
//
// Called once a day from the nightly recompute job, alongside
// RecomputeAllProgress — a real, bounded, recurring job already checking
// every storage's cached progress, not a bespoke poll added just for this.
func (s *Store) EvaluateStorageAchievements(ctx context.Context, storageID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		members, err := storageMemberIDs(ctx, tx, storageID)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}

		health, err := healthScore(ctx, tx, storageID)
		if err != nil {
			return err
		}
		if health >= gamification.ArchivistHealthScore {
			if err := awardToEveryMember(ctx, tx, storageID, members, gamification.AchArchivist, 0); err != nil {
				return err
			}
		}
		if health >= gamification.LibrarianHealthScore {
			if err := awardToEveryMember(ctx, tx, storageID, members, gamification.AchLibrarian, 0); err != nil {
				return err
			}
		}

		var productCount, categorizedCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM products WHERE storage_id = $1`, storageID).Scan(&productCount); err != nil {
			return fmt.Errorf("store: count products for achievements: %w", err)
		}
		if productCount >= gamification.DeepFreezeProductCount {
			if err := awardToEveryMember(ctx, tx, storageID, members, gamification.AchDeepFreeze, 0); err != nil {
				return err
			}
		}

		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM products WHERE storage_id = $1 AND category_id IS NOT NULL`, storageID).Scan(&categorizedCount); err != nil {
			return fmt.Errorf("store: count categorized products: %w", err)
		}
		var uncategorizedCount int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM products WHERE storage_id = $1 AND category_id IS NULL`, storageID).Scan(&uncategorizedCount); err != nil {
			return fmt.Errorf("store: count uncategorized products: %w", err)
		}
		// Strictly greater than the threshold: the 101st categorized product
		// in a fully-sorted storage is what earns it, not the 100th
		// (docs/specs/52-gamification-quests-and-ui.md's worked example).
		if uncategorizedCount == 0 && categorizedCount > gamification.SpringCleanMinCategorized {
			if err := awardToEveryMember(ctx, tx, storageID, members, gamification.AchSpringClean, 0); err != nil {
				return err
			}
		}

		if err := evaluateWellStocked(ctx, tx, storageID, members); err != nil {
			return err
		}
		return nil
	})
}

// evaluateWellStocked maintains well_stocked_since the same way
// updateCleanStreak maintains clean_since: a day with no product below its
// min_stock extends it, any product below resets it to NULL. Crossing
// WellStockedDays awards well_stocked to every member once.
func evaluateWellStocked(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, members []uuid.UUID) error {
	var anyBelowMinStock bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM products p
			 LEFT JOIN inventory_batches b ON b.product_id = p.id
			WHERE p.storage_id = $1 AND p.min_stock > 0
			GROUP BY p.id, p.min_stock
			HAVING coalesce(sum(b.quantity), 0) < p.min_stock)`, storageID).Scan(&anyBelowMinStock); err != nil {
		return fmt.Errorf("store: check well-stocked state: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO storage_gamification_settings (storage_id) VALUES ($1)
		ON CONFLICT (storage_id) DO NOTHING`, storageID); err != nil {
		return fmt.Errorf("store: ensure storage gamification settings: %w", err)
	}

	if anyBelowMinStock {
		if _, err := tx.Exec(ctx, `
			UPDATE storage_gamification_settings SET well_stocked_since = NULL, updated_at = now()
			 WHERE storage_id = $1`, storageID); err != nil {
			return fmt.Errorf("store: reset well_stocked_since: %w", err)
		}
		return nil
	}

	var since *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT well_stocked_since FROM storage_gamification_settings WHERE storage_id = $1`, storageID).Scan(&since); err != nil {
		return fmt.Errorf("store: load well_stocked_since: %w", err)
	}
	if since == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE storage_gamification_settings SET well_stocked_since = CURRENT_DATE, updated_at = now()
			 WHERE storage_id = $1`, storageID); err != nil {
			return fmt.Errorf("store: start well_stocked_since: %w", err)
		}
		return nil
	}

	days := int(time.Since(*since).Hours() / 24)
	if days < gamification.WellStockedDays {
		return nil
	}
	return awardToEveryMember(ctx, tx, storageID, members, gamification.AchWellStocked, 0)
}

// EvaluateZeroWasteWeek checks the past complete week for any item that
// passed its expiry date while stock remained (docs/specs/52-gamification-quests-and-ui.md:
// "no item passing its expiry date unconsumed"), and awards zero_waste_week
// to every member if none did. Called once a week, alongside quest
// generation, for the week that just ended.
func (s *Store) EvaluateZeroWasteWeek(ctx context.Context, storageID uuid.UUID, weekStart time.Time) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		members, err := storageMemberIDs(ctx, tx, storageID)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}

		var anyWasted bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM inventory_batches b JOIN products p ON p.id = b.product_id
				 WHERE p.storage_id = $1 AND b.quantity > 0
				   AND b.expiration_date >= $2 AND b.expiration_date < $3)`,
			storageID, weekStart.AddDate(0, 0, -7), weekStart).Scan(&anyWasted); err != nil {
			return fmt.Errorf("store: check zero-waste week: %w", err)
		}
		if anyWasted {
			return nil
		}
		return awardToEveryMember(ctx, tx, storageID, members, gamification.AchZeroWasteWeek, 0)
	})
}

// awardToEveryMember unlocks key for every member, idempotently, optionally
// paying xp once per member the first time it is unlocked — the same
// once-per-milestone-crossing shape updateCleanStreak uses.
func awardToEveryMember(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, members []uuid.UUID, key gamification.AchievementKey, xp int) error {
	for _, userID := range members {
		inserted, err := achievementNewlyUnlocked(ctx, tx, storageID, userID, string(key))
		if err != nil {
			return err
		}
		if inserted && xp > 0 {
			if err := bumpProgress(ctx, tx, storageID, userID, xp, time.Now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// UnlockedAchievement is one row of a user's achievement history in one
// storage, for the dashboard card's "recently unlocked" strip
// (docs/specs/52-gamification-quests-and-ui.md). Achievements never expire,
// so this is the caller's whole history, not a page — the frontend decides
// which are recent enough to highlight.
type UnlockedAchievement struct {
	Key        string
	UnlockedAt time.Time
}

// AchievementsForUser returns every achievement userID has unlocked in
// storageID, most recent first.
func (s *Store) AchievementsForUser(ctx context.Context, storageID, userID uuid.UUID) ([]UnlockedAchievement, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT achievement_key, unlocked_at FROM achievements_unlocked
		 WHERE storage_id = $1 AND user_id = $2 ORDER BY unlocked_at DESC`, storageID, userID)
	if err != nil {
		return nil, fmt.Errorf("store: load achievements: %w", err)
	}
	defer rows.Close()

	out := []UnlockedAchievement{}
	for rows.Next() {
		var a UnlockedAchievement
		if err := rows.Scan(&a.Key, &a.UnlockedAt); err != nil {
			return nil, fmt.Errorf("store: scan achievement: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RunNightlyStorageAchievements evaluates the storage-attributed achievements
// for every storage, once a day. Meant to be called alongside
// RecomputeAllProgress from the same nightly job
// (docs/specs/51-gamification-scoring.md's fixed 03:00 hour) — one recurring,
// bounded pass, not an additional poll.
func (s *Store) RunNightlyStorageAchievements(ctx context.Context) (int, error) {
	storages, err := s.ListStorages(ctx)
	if err != nil {
		return 0, err
	}

	n := 0
	var firstErr error
	for _, storage := range storages {
		if err := s.EvaluateStorageAchievements(ctx, storage.ID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("storage %s: %w", storage.ID, err)
			}
			continue
		}
		n++
	}
	return n, firstErr
}
