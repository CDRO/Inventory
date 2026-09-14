package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/gamification"
)

// Quest is one weekly quest, generated from live storage state
// (docs/specs/52-gamification-quests-and-ui.md). Progress is computed live
// against the ids frozen into Params at generation time, never cached, so it
// always reflects the ledger exactly.
type Quest struct {
	ID          uuid.UUID
	StorageID   uuid.UUID
	WeekStart   time.Time
	Generator   gamification.GeneratorKind
	Params      json.RawMessage
	TargetCount int
	XPReward    int
	CompletedAt *time.Time
	Progress    int
}

// QuestsSnapshot is what the dashboard card and the header badge need for
// one storage's current week (docs/specs/52-gamification-quests-and-ui.md):
// this week's quests, and — when there are none — the all-clear read with
// its clean-streak counter, since zero quests must never render as an empty
// list.
type QuestsSnapshot struct {
	WeekStart       time.Time
	Quests          []Quest
	CleanStreakDays int
	// IsComeback is true for the first 7 days after quests reappear
	// following a gap of two or more all-clear weeks
	// (docs/specs/52-gamification-quests-and-ui.md's "coming back from a
	// quiet stretch").
	IsComeback bool
	// WeeklyGoalTarget and WeeklyGoalCurrent back the storage goal bar's
	// "14 of 20 this week" caption — the co-op frame, never a ranking
	// (docs/specs/52-gamification-quests-and-ui.md).
	WeeklyGoalTarget  int
	WeeklyGoalCurrent int
}

// idListParams is the shared params shape for every generator whose target
// is a fixed set of ids resolved at generation time: missing_expiry,
// uncategorized, imageless, untracked_reorder and expiring_soon.
type idListParams struct {
	IDs []uuid.UUID `json:"ids"`
}

type staleLocationParams struct {
	LocationID uuid.UUID `json:"location_id"`
	// LocationName and SinceMonth are resolved at generation time so the
	// frontend can build "Re-scan Basement — untouched since June"
	// (docs/specs/52-gamification-quests-and-ui.md) from generator + params
	// alone, with no second lookup. SinceMonth is empty when the location has
	// never had any activity at all.
	LocationName string `json:"location_name"`
	SinceMonth   string `json:"since_month,omitempty"`
}

type firstMileParams struct {
	// StartCount is the product count at generation time, so progress can be
	// "how many new products since then" rather than a moving target if the
	// storage's count changes mid-week for unrelated reasons (a deletion).
	StartCount int `json:"start_count"`
}

// consumptionHygieneQuestTarget is fixed at one qualifying week: the
// generator's condition is binary (any consumption logged, or not), so there
// is nothing to count toward beyond the first.
const consumptionHygieneQuestTarget = 1

// staleLocationQuestTarget is likewise binary: one re-scan resolves it.
const staleLocationQuestTarget = 1

// missingExpiryThreshold etc. are the fixed candidate-set sizes from
// docs/specs/52-gamification-quests-and-ui.md's generator table — the
// rendered text ("5 fresh items") is a fixed number, not however many
// currently qualify.
const (
	missingExpiryThreshold    = 5
	uncategorizedThreshold    = 5
	imagelessThreshold        = 5
	untrackedReorderThreshold = 5
	expiringSoonThreshold     = 3
	firstMileProductCeiling   = 10
	staleLocationDays         = 90
	consumptionHygieneDays    = 14
)

// GenerateWeeklyQuests evaluates every generator against storageID's live
// state, picks up to three, and writes them for weekStart — idempotently:
// the UNIQUE (storage_id, week_start, generator) constraint means calling
// this twice for a week that was already generated is a no-op for whichever
// generators already have a row.
//
// It also maintains clean_since (docs/specs/52-gamification-quests-and-ui.md):
// a week where nothing fired extends the clean streak; any generator firing
// resets it. Crossing a clean-storage milestone pays every current member.
func (s *Store) GenerateWeeklyQuests(ctx context.Context, storageID uuid.UUID, weekStart time.Time) error {
	weekStart = mondayOf(weekStart)

	return s.inTx(ctx, func(tx pgx.Tx) error {
		candidates, err := gatherQuestCandidates(ctx, tx, storageID)
		if err != nil {
			return err
		}
		selected := gamification.SelectQuests(candidates)

		for _, c := range selected {
			id, err := newID()
			if err != nil {
				return err
			}
			paramsJSON, err := json.Marshal(c.Params)
			if err != nil {
				return fmt.Errorf("store: marshal quest params: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO quests (id, storage_id, week_start, generator, params, target_count, xp_reward)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (storage_id, week_start, generator) DO NOTHING`,
				id, storageID, weekStart, string(c.Generator), paramsJSON, c.TargetCount, gamification.XPForGenerator(c.Generator)); err != nil {
				return fmt.Errorf("store: insert quest: %w", err)
			}
		}

		return updateCleanStreak(ctx, tx, storageID, weekStart, len(selected) == 0)
	})
}

// gatherQuestCandidates evaluates every generator's live condition
// (docs/specs/52-gamification-quests-and-ui.md's table), each independent of
// the others so a failure in one query is just that generator not
// contributing rather than the whole week failing to generate.
func gatherQuestCandidates(ctx context.Context, tx pgx.Tx, storageID uuid.UUID) ([]gamification.Candidate, error) {
	candidates := make([]gamification.Candidate, 0, 8)

	add := func(c gamification.Candidate, err error) error {
		if err != nil {
			return err
		}
		candidates = append(candidates, c)
		return nil
	}

	if err := add(staleLocationCandidate(ctx, tx, storageID)); err != nil {
		return nil, err
	}
	if err := add(idListCandidate(ctx, tx, storageID, gamification.GeneratorMissingExpiry, missingExpiryThreshold, `
		SELECT b.id FROM inventory_batches b JOIN products p ON p.id = b.product_id
		 WHERE p.storage_id = $1 AND p.item_type IN ('perishable', 'long_shelf_life')
		   AND b.expiration_date IS NULL AND b.quantity > 0
		 ORDER BY b.id`)); err != nil {
		return nil, err
	}
	if err := add(idListCandidate(ctx, tx, storageID, gamification.GeneratorUncategorized, uncategorizedThreshold, `
		SELECT id FROM products WHERE storage_id = $1 AND category_id IS NULL ORDER BY id`)); err != nil {
		return nil, err
	}
	if err := add(idListCandidate(ctx, tx, storageID, gamification.GeneratorImageless, imagelessThreshold, `
		SELECT id FROM products WHERE storage_id = $1 AND image_url IS NULL AND icon_name IS NULL
		 ORDER BY id`)); err != nil {
		return nil, err
	}
	if err := add(idListCandidate(ctx, tx, storageID, gamification.GeneratorUntrackedReorder, untrackedReorderThreshold, `
		SELECT id FROM products WHERE storage_id = $1 AND min_stock = 0 ORDER BY id`)); err != nil {
		return nil, err
	}
	if err := add(idListCandidate(ctx, tx, storageID, gamification.GeneratorExpiringSoon, expiringSoonThreshold, `
		SELECT b.id FROM inventory_batches b JOIN products p ON p.id = b.product_id
		 WHERE p.storage_id = $1 AND b.quantity > 0
		   AND b.expiration_date >= CURRENT_DATE AND b.expiration_date < CURRENT_DATE + 3
		 ORDER BY b.expiration_date`)); err != nil {
		return nil, err
	}
	if err := add(consumptionHygieneCandidate(ctx, tx, storageID)); err != nil {
		return nil, err
	}
	if err := add(firstMileCandidate(ctx, tx, storageID)); err != nil {
		return nil, err
	}

	return candidates, nil
}

// idListCandidate is the shared shape for every generator whose fire
// condition is "at least threshold rows match query": query returns every
// matching row (no LIMIT), so Need reflects how many actually qualify —
// severity, not just whether the generator cleared its threshold — while
// only the first threshold of those ids are frozen into the quest's target.
func idListCandidate(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, generator gamification.GeneratorKind, threshold int, query string) (gamification.Candidate, error) {
	rows, err := tx.Query(ctx, query, storageID)
	if err != nil {
		return gamification.Candidate{}, fmt.Errorf("store: gather %s candidates: %w", generator, err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return gamification.Candidate{}, fmt.Errorf("store: scan %s candidate: %w", generator, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return gamification.Candidate{}, fmt.Errorf("store: gather %s candidates: %w", generator, err)
	}

	if len(ids) < threshold {
		return gamification.Candidate{Generator: generator, Fires: false}, nil
	}
	need := float64(len(ids))
	ids = ids[:threshold]

	return gamification.Candidate{
		Generator:   generator,
		Fires:       true,
		Need:        need,
		TargetCount: len(ids),
		Params:      idListParams{IDs: ids},
	}, nil
}

func staleLocationCandidate(ctx context.Context, tx pgx.Tx, storageID uuid.UUID) (gamification.Candidate, error) {
	var (
		locationID   uuid.UUID
		locationName string
		lastActive   *time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT l.id, l.name, MAX(il.timestamp)
		  FROM locations l
		  LEFT JOIN inventory_batches b ON b.location_id = l.id
		  LEFT JOIN inventory_logs il ON il.batch_id = b.id
		 WHERE l.storage_id = $1 AND EXISTS (SELECT 1 FROM inventory_batches bb WHERE bb.location_id = l.id)
		 GROUP BY l.id, l.name
		HAVING MAX(il.timestamp) IS NULL OR MAX(il.timestamp) < now() - make_interval(days => $2)
		 ORDER BY MAX(il.timestamp) ASC NULLS FIRST
		 LIMIT 1`, storageID, staleLocationDays).Scan(&locationID, &locationName, &lastActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return gamification.Candidate{Generator: gamification.GeneratorStaleLocation, Fires: false}, nil
	}
	if err != nil {
		return gamification.Candidate{}, fmt.Errorf("store: gather stale_location candidate: %w", err)
	}

	need := float64(staleLocationDays)
	sinceMonth := ""
	if lastActive != nil {
		need = time.Since(*lastActive).Hours() / 24
		sinceMonth = lastActive.Month().String()
	}
	return gamification.Candidate{
		Generator:   gamification.GeneratorStaleLocation,
		Fires:       true,
		Need:        need,
		TargetCount: staleLocationQuestTarget,
		Params:      staleLocationParams{LocationID: locationID, LocationName: locationName, SinceMonth: sinceMonth},
	}, nil
}

func consumptionHygieneCandidate(ctx context.Context, tx pgx.Tx, storageID uuid.UUID) (gamification.Candidate, error) {
	var anyoneOnHoliday bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM holiday_weeks hw
			 JOIN storage_members m ON m.user_id = hw.user_id
			WHERE m.storage_id = $1 AND hw.week_start = $2)`,
		storageID, mondayOf(time.Now())).Scan(&anyoneOnHoliday); err != nil {
		return gamification.Candidate{}, fmt.Errorf("store: check holiday suppression: %w", err)
	}
	if anyoneOnHoliday {
		return gamification.Candidate{Generator: gamification.GeneratorConsumptionHygiene, Fires: false}, nil
	}

	var lastConsumption *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT MAX(il.timestamp) FROM inventory_logs il JOIN products p ON p.id = il.product_id
		 WHERE p.storage_id = $1 AND il.reason = 'consumption'`, storageID).Scan(&lastConsumption); err != nil {
		return gamification.Candidate{}, fmt.Errorf("store: gather consumption_hygiene candidate: %w", err)
	}

	need := float64(consumptionHygieneDays)
	fires := lastConsumption == nil
	if lastConsumption != nil {
		days := time.Since(*lastConsumption).Hours() / 24
		need = days
		fires = days >= consumptionHygieneDays
	}
	if !fires {
		return gamification.Candidate{Generator: gamification.GeneratorConsumptionHygiene, Fires: false}, nil
	}
	return gamification.Candidate{
		Generator:   gamification.GeneratorConsumptionHygiene,
		Fires:       true,
		Need:        need,
		TargetCount: consumptionHygieneQuestTarget,
		Params:      struct{}{},
	}, nil
}

func firstMileCandidate(ctx context.Context, tx pgx.Tx, storageID uuid.UUID) (gamification.Candidate, error) {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM products WHERE storage_id = $1`, storageID).Scan(&count); err != nil {
		return gamification.Candidate{}, fmt.Errorf("store: gather first_mile candidate: %w", err)
	}
	if count >= firstMileProductCeiling {
		return gamification.Candidate{Generator: gamification.GeneratorFirstMile, Fires: false}, nil
	}
	return gamification.Candidate{
		Generator:   gamification.GeneratorFirstMile,
		Fires:       true,
		Need:        float64(firstMileProductCeiling-count) * 10, // onboarding weighted to usually win
		TargetCount: firstMileProductCeiling - count,
		Params:      firstMileParams{StartCount: count},
	}, nil
}

// updateCleanStreak maintains storage_gamification_settings.clean_since
// (docs/specs/52-gamification-quests-and-ui.md): a week with nothing firing
// starts or extends the streak; any generator firing resets it. Crossing a
// fixed milestone pays every current member once, guarded by
// achievements_unlocked's primary key so a re-run of the same week's
// generation cannot pay twice.
func updateCleanStreak(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, weekStart time.Time, allClear bool) error {
	var cleanSince *time.Time
	if _, err := tx.Exec(ctx, `
		INSERT INTO storage_gamification_settings (storage_id) VALUES ($1)
		ON CONFLICT (storage_id) DO NOTHING`, storageID); err != nil {
		return fmt.Errorf("store: ensure storage gamification settings: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT clean_since FROM storage_gamification_settings WHERE storage_id = $1`, storageID).Scan(&cleanSince); err != nil {
		return fmt.Errorf("store: load clean_since: %w", err)
	}

	if !allClear {
		if cleanSince != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE storage_gamification_settings SET clean_since = NULL, updated_at = now()
				 WHERE storage_id = $1`, storageID); err != nil {
				return fmt.Errorf("store: reset clean_since: %w", err)
			}
		}
		return nil
	}

	if cleanSince == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE storage_gamification_settings SET clean_since = $2, updated_at = now()
			 WHERE storage_id = $1`, storageID, weekStart); err != nil {
			return fmt.Errorf("store: start clean_since: %w", err)
		}
		return nil
	}

	cleanDays := int(weekStart.Sub(*cleanSince).Hours() / 24)
	milestones := gamification.ImmaculateMilestonesReached(cleanDays)
	if len(milestones) == 0 {
		return nil
	}

	members, err := storageMemberIDs(ctx, tx, storageID)
	if err != nil {
		return err
	}
	for _, m := range milestones {
		for _, userID := range members {
			// Paying every milestone on every generation once clean_since is
			// old enough would re-pay members each week.
			// achievements_unlocked's primary key makes this insert a no-op
			// on a repeat, so the XP bump below only runs the one time it
			// actually inserts a new row.
			inserted, err := achievementNewlyUnlocked(ctx, tx, storageID, userID, m.Key)
			if err != nil {
				return err
			}
			if inserted {
				if err := bumpProgress(ctx, tx, storageID, userID, m.XP, time.Now()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func storageMemberIDs(ctx context.Context, tx pgx.Tx, storageID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT user_id FROM storage_members WHERE storage_id = $1`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: load storage members: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan storage member: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// QuestsForStorage reads this week's quests, with live-recomputed progress,
// plus the all-clear read when there are none
// (docs/specs/52-gamification-quests-and-ui.md).
func (s *Store) QuestsForStorage(ctx context.Context, storageID uuid.UUID) (*QuestsSnapshot, error) {
	weekStart := mondayOf(time.Now())
	snap := &QuestsSnapshot{WeekStart: weekStart}

	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_id, week_start, generator, params, target_count, xp_reward, completed_at
		  FROM quests WHERE storage_id = $1 AND week_start = $2 ORDER BY xp_reward DESC`,
		storageID, weekStart)
	if err != nil {
		return nil, fmt.Errorf("store: load quests: %w", err)
	}
	var quests []Quest
	for rows.Next() {
		var q Quest
		var generator string
		if err := rows.Scan(&q.ID, &q.StorageID, &q.WeekStart, &generator, &q.Params, &q.TargetCount, &q.XPReward, &q.CompletedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan quest: %w", err)
		}
		q.Generator = gamification.GeneratorKind(generator)
		quests = append(quests, q)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: load quests: %w", err)
	}
	rows.Close()

	for i := range quests {
		if quests[i].CompletedAt != nil {
			quests[i].Progress = quests[i].TargetCount
			continue
		}
		progress, err := questProgress(ctx, s.pool, storageID, quests[i])
		if err != nil {
			return nil, err
		}
		quests[i].Progress = progress
	}
	snap.Quests = quests

	var cleanSince *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT clean_since FROM storage_gamification_settings WHERE storage_id = $1`, storageID).Scan(&cleanSince); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: load clean_since: %w", err)
	}
	if cleanSince != nil {
		snap.CleanStreakDays = int(time.Since(*cleanSince).Hours() / 24)
	}

	if len(quests) > 0 {
		var previousWeekCount, priorWeekCount int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM quests WHERE storage_id=$1 AND week_start=$2`,
			storageID, weekStart.AddDate(0, 0, -7)).Scan(&previousWeekCount); err != nil {
			return nil, fmt.Errorf("store: check prior week quests: %w", err)
		}
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM quests WHERE storage_id=$1 AND week_start=$2`,
			storageID, weekStart.AddDate(0, 0, -14)).Scan(&priorWeekCount); err != nil {
			return nil, fmt.Errorf("store: check prior week quests: %w", err)
		}
		snap.IsComeback = previousWeekCount == 0 && priorWeekCount == 0
	}

	settings, err := s.StorageGamificationSettingsFor(ctx, storageID)
	if err != nil {
		return nil, err
	}
	snap.WeeklyGoalTarget = settings.WeeklyGoalItems

	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT l.product_id) FROM inventory_logs l JOIN products p ON p.id = l.product_id
		 WHERE p.storage_id = $1 AND l.reason IN ('purchase', 'consumption', 'vision_ingestion')
		   AND l.timestamp >= $2`, storageID, weekStart).Scan(&snap.WeeklyGoalCurrent); err != nil {
		return nil, fmt.Errorf("store: compute weekly goal progress: %w", err)
	}

	return snap, nil
}

// questProgress re-evaluates one quest's completion condition live, against
// exactly the ids frozen into its params at generation time
// (docs/specs/52-gamification-quests-and-ui.md: "Progress is evaluated
// server-side from the same ledgers as XP").
func questProgress(ctx context.Context, q querier, storageID uuid.UUID, quest Quest) (int, error) {
	switch quest.Generator {
	case gamification.GeneratorMissingExpiry:
		return countMatching(ctx, q, quest.Params, `
			SELECT count(*) FROM inventory_batches WHERE id = ANY($1) AND expiration_date IS NOT NULL`)
	case gamification.GeneratorUncategorized:
		return countMatching(ctx, q, quest.Params, `
			SELECT count(*) FROM products WHERE id = ANY($1) AND category_id IS NOT NULL`)
	case gamification.GeneratorImageless:
		return countMatching(ctx, q, quest.Params, `
			SELECT count(*) FROM products WHERE id = ANY($1) AND (image_url IS NOT NULL OR icon_name IS NOT NULL)`)
	case gamification.GeneratorUntrackedReorder:
		return countMatching(ctx, q, quest.Params, `
			SELECT count(*) FROM products WHERE id = ANY($1) AND min_stock > 0`)
	case gamification.GeneratorExpiringSoon:
		return countMatching(ctx, q, quest.Params, `
			SELECT count(*) FROM inventory_batches WHERE id = ANY($1) AND (quantity <= 0 OR expiration_date < CURRENT_DATE OR expiration_date >= CURRENT_DATE + 3)`)
	case gamification.GeneratorStaleLocation:
		var params staleLocationParams
		if err := json.Unmarshal(quest.Params, &params); err != nil {
			return 0, fmt.Errorf("store: unmarshal stale_location params: %w", err)
		}
		var touched bool
		if err := q.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM inventory_logs il JOIN inventory_batches b ON b.id = il.batch_id
				 WHERE b.location_id = $1 AND il.timestamp >= $2)`,
			params.LocationID, quest.WeekStart).Scan(&touched); err != nil {
			return 0, fmt.Errorf("store: check stale_location progress: %w", err)
		}
		if touched {
			return 1, nil
		}
		return 0, nil
	case gamification.GeneratorConsumptionHygiene:
		var logged bool
		if err := q.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM inventory_logs il JOIN products p ON p.id = il.product_id
				 WHERE p.storage_id = $1 AND il.reason = 'consumption' AND il.timestamp >= $2)`,
			storageID, quest.WeekStart).Scan(&logged); err != nil {
			return 0, fmt.Errorf("store: check consumption_hygiene progress: %w", err)
		}
		if logged {
			return 1, nil
		}
		return 0, nil
	case gamification.GeneratorFirstMile:
		var params firstMileParams
		if err := json.Unmarshal(quest.Params, &params); err != nil {
			return 0, fmt.Errorf("store: unmarshal first_mile params: %w", err)
		}
		var count int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM products WHERE storage_id = $1`, storageID).Scan(&count); err != nil {
			return 0, fmt.Errorf("store: check first_mile progress: %w", err)
		}
		progress := count - params.StartCount
		if progress < 0 {
			progress = 0
		}
		if progress > quest.TargetCount {
			progress = quest.TargetCount
		}
		return progress, nil
	default:
		return 0, nil
	}
}

func countMatching(ctx context.Context, q querier, rawParams json.RawMessage, query string) (int, error) {
	var params idListParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return 0, fmt.Errorf("store: unmarshal quest params: %w", err)
	}
	var n int
	if err := q.QueryRow(ctx, query, params.IDs).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: quest progress query: %w", err)
	}
	return n, nil
}

// questRelevantGenerators is the coarse kind/reason → generator relevance map
// advanceQuests uses to decide which active quests a given write could
// plausibly advance. This is a deliberate simplification of "a qualifying
// action is any event the quest's progress counter counted"
// (docs/specs/52-gamification-quests-and-ui.md): rather than threading each
// write's exact target id through to quest matching, any write of a kind
// that generator cares about counts as an attempt, and questProgress above
// is the actual source of truth for whether the quest is complete. See the
// PR description for the follow-up that would tighten this to per-id
// matching.
var questRelevantGenerators = map[string][]gamification.GeneratorKind{
	string(ReasonPurchase):                   {gamification.GeneratorFirstMile, gamification.GeneratorStaleLocation},
	string(ReasonVisionIngestion):            {gamification.GeneratorFirstMile, gamification.GeneratorStaleLocation},
	string(ReasonConsumption):                {gamification.GeneratorConsumptionHygiene, gamification.GeneratorExpiringSoon},
	string(gamification.KindExpiryConfirmed): {gamification.GeneratorMissingExpiry},
	string(gamification.KindMetadataFilled):  {gamification.GeneratorUncategorized, gamification.GeneratorImageless, gamification.GeneratorUntrackedReorder},
	string(gamification.KindLocationMapped):  {gamification.GeneratorStaleLocation},
}

// advanceQuests re-checks every active quest this coarse write kind is
// relevant to, records userID as a contributor, and pays out on completion.
// Called from recordContribution and bumpForLedgerReason — the two seams
// every scoring-relevant write already funnels through
// (docs/specs/51-gamification-scoring.md) — so no additional call sites are
// needed at the individual write paths.
func advanceQuests(ctx context.Context, tx pgx.Tx, storageID, userID uuid.UUID, kind string) error {
	generators := questRelevantGenerators[kind]
	if len(generators) == 0 {
		return nil
	}

	weekStart := mondayOf(time.Now())
	for _, generator := range generators {
		var quest Quest
		var generatorStr string
		err := tx.QueryRow(ctx, `
			SELECT id, storage_id, week_start, generator, params, target_count, xp_reward, completed_at
			  FROM quests
			 WHERE storage_id = $1 AND week_start = $2 AND generator = $3 AND completed_at IS NULL
			 FOR UPDATE`,
			storageID, weekStart, string(generator)).Scan(
			&quest.ID, &quest.StorageID, &quest.WeekStart, &generatorStr, &quest.Params, &quest.TargetCount, &quest.XPReward, &quest.CompletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("store: lock active quest: %w", err)
		}
		quest.Generator = gamification.GeneratorKind(generatorStr)

		if _, err := tx.Exec(ctx, `
			INSERT INTO quest_contributors (quest_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			quest.ID, userID); err != nil {
			return fmt.Errorf("store: record quest contributor: %w", err)
		}

		progress, err := questProgress(ctx, tx, storageID, quest)
		if err != nil {
			return err
		}
		if progress < quest.TargetCount {
			continue
		}

		tag, err := tx.Exec(ctx, `
			UPDATE quests SET completed_at = now() WHERE id = $1 AND completed_at IS NULL`, quest.ID)
		if err != nil {
			return fmt.Errorf("store: complete quest: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue // another concurrent write completed it first
		}

		contributors, err := tx.Query(ctx, `SELECT user_id FROM quest_contributors WHERE quest_id = $1`, quest.ID)
		if err != nil {
			return fmt.Errorf("store: load quest contributors: %w", err)
		}
		var contributorIDs []uuid.UUID
		for contributors.Next() {
			var id uuid.UUID
			if err := contributors.Scan(&id); err != nil {
				contributors.Close()
				return fmt.Errorf("store: scan quest contributor: %w", err)
			}
			contributorIDs = append(contributorIDs, id)
		}
		if err := contributors.Err(); err != nil {
			contributors.Close()
			return fmt.Errorf("store: load quest contributors: %w", err)
		}
		contributors.Close()

		for _, contributorID := range contributorIDs {
			if err := bumpProgress(ctx, tx, storageID, contributorID, quest.XPReward, time.Now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// RunWeeklyGamificationJobs generates this week's quests and evaluates the
// week that just ended for zero_waste_week, for every storage
// (docs/specs/52-gamification-quests-and-ui.md: "generated Monday 00:00 in
// the server's local timezone"). It is meant to be called once, at that
// moment, by the same kind of always-on background loop
// runGamificationRecompute already is for the nightly XP recompute — a
// scheduled job tied to real weekly state, not a poll.
//
// One storage's failure does not stop the rest: a household's quests failing
// to generate must not also block every other household's.
func (s *Store) RunWeeklyGamificationJobs(ctx context.Context, now time.Time) (int, error) {
	storages, err := s.ListStorages(ctx)
	if err != nil {
		return 0, err
	}

	weekStart := mondayOf(now)
	n := 0
	var firstErr error
	for _, storage := range storages {
		if err := s.GenerateWeeklyQuests(ctx, storage.ID, weekStart); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("storage %s: %w", storage.ID, err)
			}
			continue
		}
		if err := s.EvaluateZeroWasteWeek(ctx, storage.ID, weekStart); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("storage %s: %w", storage.ID, err)
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// unlockAchievement records key as unlocked for (storageID, userID), a no-op
// if it already is (docs/specs/52-gamification-quests-and-ui.md: achievements
// never expire and unlock once).
func unlockAchievement(ctx context.Context, tx pgx.Tx, storageID, userID uuid.UUID, key string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO achievements_unlocked (storage_id, user_id, achievement_key)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, storageID, userID, key)
	if err != nil {
		return fmt.Errorf("store: unlock achievement %s: %w", key, err)
	}
	return nil
}

// achievementNewlyUnlocked reports whether key is unlocked for
// (storageID, userID) — used to gate a one-time XP payment onto the same
// insert unlockAchievement performs, without a second race-prone read.
func achievementNewlyUnlocked(ctx context.Context, tx pgx.Tx, storageID, userID uuid.UUID, key string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO achievements_unlocked (storage_id, user_id, achievement_key)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, storageID, userID, key)
	if err != nil {
		return false, fmt.Errorf("store: unlock achievement %s: %w", key, err)
	}
	return tag.RowsAffected() > 0, nil
}
