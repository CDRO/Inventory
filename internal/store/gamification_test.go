package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/gamification"
	"github.com/CDRO/Inventory/internal/store"
)

// progressRow reads the raw user_progress row, or the zero value if none
// exists — countRows-style, but this test file needs the actual XP too.
func progressRow(t *testing.T, ctx context.Context, storageID, userID uuid.UUID) (xp, level, streak int, found bool) {
	t.Helper()
	err := testPool.QueryRow(ctx, `
		SELECT xp, level, streak_weeks FROM user_progress WHERE storage_id = $1 AND user_id = $2`,
		storageID, userID).Scan(&xp, &level, &streak)
	if err != nil {
		return 0, 0, 0, false
	}
	return xp, level, streak, true
}

func contributionCount(t *testing.T, ctx context.Context, storageID, userID uuid.UUID, kind gamification.ContributionKind) int {
	t.Helper()
	return countRows(t, ctx,
		`SELECT count(*) FROM contribution_events WHERE storage_id = $1 AND user_id = $2 AND kind = $3`,
		storageID, userID, string(kind))
}

// TestCreateBatchBumpsProgressForVisionIngestion covers the live write-path
// hook that never touches inventory: confirming a shelf-photo proposal earns
// XP through the same createBatch call every other write goes through.
func TestCreateBatchBumpsProgressForVisionIngestion(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 6,
		Reason: store.ReasonVisionIngestion, CreatedBy: &userID,
	})
	require.NoError(t, err)

	xp, level, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found, "the first scored action must create the progress row")
	assert.Equal(t, gamification.XPLedgerContribution, xp)
	assert.Equal(t, gamification.Level(gamification.XPLedgerContribution), level)
}

// TestAdjustBatchBumpsProgressForConsumption checks the decrement path pays
// the same as the increment path — deliberately, per
// docs/specs/51-gamification-scoring.md.
func TestAdjustBatchBumpsProgressForConsumption(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 6)
	userID := newUser(t, ctx)

	require.NoError(t, s.AdjustBatch(ctx, storageID, batchID, -6, store.ReasonConsumption, &userID))

	xp, _, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, gamification.XPLedgerContribution, xp)
	_ = productID
}

// TestMoveAndAuditReasonsEarnNoXP: a move relocates stock without adding,
// removing, or improving anything about it, and an audit row corrects the
// ledger rather than recording new work — neither is in the XP table.
func TestMoveAndAuditReasonsEarnNoXP(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, batchID := stocked(t, ctx, s, 6)
	userID := newUser(t, ctx)
	kitchen, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)

	_, err = moveBatch(ctx, s, storageID, batchID, kitchen.ID, &userID)
	require.NoError(t, err)
	require.NoError(t, s.AdjustBatch(ctx, storageID, batchID, -1, store.ReasonAudit, &userID))

	_, _, _, found := progressRow(t, ctx, storageID, userID)
	assert.False(t, found, "move and audit reasons must never create a progress row")
}

// TestNilUserEarnsNoXP: a system-attributed change earns nobody XP, which is
// the correct behaviour rather than an error condition.
func TestNilUserEarnsNoXP(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)

	n := countRows(t, ctx, `SELECT count(*) FROM user_progress WHERE storage_id = $1`, storageID)
	assert.Equal(t, 0, n)
}

// TestProgressAccumulatesAcrossDistinctProducts: two distinct products each
// earn their own credit, and the level recomputes from the running total.
func TestProgressAccumulatesAcrossDistinctProducts(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	for range 17 {
		product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Product"})
		require.NoError(t, err)
		_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
			ProductID: product.ID, LocationID: location.ID, Quantity: 1,
			Reason: store.ReasonPurchase, CreatedBy: &userID,
		})
		require.NoError(t, err)
	}

	xp, level, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, 17*gamification.XPLedgerContribution, xp)
	assert.Equal(t, gamification.Level(xp), level)
}

// TestCreateLocationAsUserRecordsLocationMapped covers the location_mapped
// hook end to end: a contribution row with the new location as its ref, and
// the matching XP applied to user_progress.
func TestCreateLocationAsUserRecordsLocationMapped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	loc, err := s.CreateLocationAsUser(ctx, storageID, store.NewLocation{Name: "Attic"}, userID)
	require.NoError(t, err)

	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindLocationMapped))
	var ref uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT ref_id FROM contribution_events WHERE storage_id = $1 AND kind = 'location_mapped'`, storageID).Scan(&ref))
	assert.Equal(t, loc.ID, ref)

	xp, _, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, gamification.XPLocationMapped, xp)
}

// TestUpdateProductMinStockAsUserFillsGapOnly: raising an already-tracked
// threshold, or lowering one, earns nothing — only closing a gap that was
// previously zero does.
func TestUpdateProductMinStockAsUserFillsGapOnly(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Rice"})
	require.NoError(t, err)

	_, err = s.UpdateProductMinStockAsUser(ctx, storageID, product.ID, 3, userID)
	require.NoError(t, err)
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled), "0 -> 3 fills a gap")

	_, err = s.UpdateProductMinStockAsUser(ctx, storageID, product.ID, 5, userID)
	require.NoError(t, err)
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled), "3 -> 5 tunes an existing threshold, no new credit")

	_, err = s.UpdateProductMinStockAsUser(ctx, storageID, product.ID, 0, userID)
	require.NoError(t, err)
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindMetadataFilled), "lowering to 0 is not a gap filled")
}

// TestSetBatchExpirationRecordsExpiryConfirmed checks the hook fires with an
// acting user and does not when the caller is nil (a system-driven reset).
func TestSetBatchExpirationRecordsExpiryConfirmed(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, batchID := stocked(t, ctx, s, 1)
	userID := newUser(t, ctx)
	when := time.Now().AddDate(0, 0, 30)

	_, err := s.SetBatchExpiration(ctx, storageID, batchID, &when, &userID)
	require.NoError(t, err)
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindExpiryConfirmed))

	otherUser := newUser(t, ctx)
	_, err = s.SetBatchExpiration(ctx, storageID, batchID, &when, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, contributionCount(t, ctx, storageID, otherUser, gamification.KindExpiryConfirmed))
}

// TestResolveShoppingListItemRecordsAmbiguityResolvedOnlyFromAmbiguous: an
// exact-match line that gets resolved is not "ambiguity resolved" — only a
// line that was actually ambiguous earns the contribution.
func TestResolveShoppingListItemRecordsAmbiguityResolvedOnlyFromAmbiguous(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	_, items, err := s.CreateShoppingList(ctx, storageID, store.SourceText, &userID, []store.NewShoppingListItem{
		{RawText: "cheese", Status: store.ItemAmbiguous},
		{RawText: "milk", Status: store.ItemExactMatch},
	})
	require.NoError(t, err)

	_, err = s.ResolveShoppingListItem(ctx, storageID, items[0].ID, store.ResolveLine{}, &userID)
	require.NoError(t, err)
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindAmbiguityResolved))

	_, err = s.ResolveShoppingListItem(ctx, storageID, items[1].ID, store.ResolveLine{}, &userID)
	require.NoError(t, err)
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindAmbiguityResolved), "resolving an exact-match line is not a correction")
}

// ingestProposalRow is the subset of internal/ingest.Row this test needs to
// build a stored proposal with a confident exact_match, so ConfirmIngestion's
// ai_correction detection has something to compare against.
type ingestProposalRow struct {
	RowID string `json:"row_id"`
	Match struct {
		Status  string `json:"status"`
		Product *struct {
			ID uuid.UUID `json:"id"`
		} `json:"product"`
	} `json:"match"`
}

func exactMatchProposal(t *testing.T, rowID string, proposed uuid.UUID) json.RawMessage {
	t.Helper()
	row := ingestProposalRow{RowID: rowID}
	row.Match.Status = "exact_match"
	row.Match.Product = &struct {
		ID uuid.UUID `json:"id"`
	}{ID: proposed}
	payload, err := json.Marshal(struct {
		Rows []ingestProposalRow `json:"rows"`
	}{Rows: []ingestProposalRow{row}})
	require.NoError(t, err)
	return payload
}

// exactMatchProposalWithASecondRow is exactMatchProposal plus a second,
// plain row with no match info — for tests that need matchRowIDs to accept a
// two-row decision list so the second row's own validation is what fails,
// rather than the row-id-set check rejecting the request before either row
// is ever processed.
func exactMatchProposalWithASecondRow(t *testing.T, firstRowID string, proposed uuid.UUID, secondRowID string) json.RawMessage {
	t.Helper()
	first := ingestProposalRow{RowID: firstRowID}
	first.Match.Status = "exact_match"
	first.Match.Product = &struct {
		ID uuid.UUID `json:"id"`
	}{ID: proposed}
	second := ingestProposalRow{RowID: secondRowID}

	payload, err := json.Marshal(struct {
		Rows []ingestProposalRow `json:"rows"`
	}{Rows: []ingestProposalRow{first, second}})
	require.NoError(t, err)
	return payload
}

// TestConfirmIngestionRecordsAICorrectionWhenTheReviewerOverrides checks the
// comparison against the stored proposal, not just "a batch got created".
func TestConfirmIngestionRecordsAICorrectionWhenTheReviewerOverrides(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	proposed, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Ketchup"})
	require.NoError(t, err)
	actual, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Barbecue Sauce"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, job.ID, exactMatchProposal(t, "0", proposed.ID)))

	_, err = s.ConfirmIngestion(ctx, storageID, job.ID, &userID, []store.IngestDecision{
		{RowID: "0", Accept: true, ProductID: &actual.ID, Quantity: 1, LocationID: &location.ID},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindAICorrection))
}

// TestConfirmIngestionRecordsNoCorrectionWhenAccepted is the control: filing
// the batch against exactly what the model proposed is acceptance, not a
// correction, and must not earn the higher credit.
func TestConfirmIngestionRecordsNoCorrectionWhenAccepted(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	proposed, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Ketchup"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, job.ID, exactMatchProposal(t, "0", proposed.ID)))

	_, err = s.ConfirmIngestion(ctx, storageID, job.ID, &userID, []store.IngestDecision{
		{RowID: "0", Accept: true, ProductID: &proposed.ID, Quantity: 1, LocationID: &location.ID},
	})
	require.NoError(t, err)

	assert.Equal(t, 0, contributionCount(t, ctx, storageID, userID, gamification.KindAICorrection))
}

// TestRecomputeAllProgressRebuildsFromScratch: wiping the cache and rebuilding
// it from the two ledgers must land on exactly the same total the live path
// produced, for the simple case with no coalescing in play.
func TestRecomputeAllProgressRebuildsFromScratch(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, _ := stocked(t, ctx, s, 6)
	userID := newUser(t, ctx)

	loc, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Kitchen"})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Oats"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: loc.ID, Quantity: 1, Reason: store.ReasonPurchase, CreatedBy: &userID,
	})
	require.NoError(t, err)

	before, _, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)

	_, err = testPool.Exec(ctx, `DELETE FROM user_progress WHERE storage_id = $1`, storageID)
	require.NoError(t, err)

	n, err := s.RecomputeAllProgress(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)

	after, level, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found, "recompute must recreate the row, not just refuse to delete it")
	assert.Equal(t, before, after, "recompute from scratch must reach the same total the live path reached")
	assert.Equal(t, gamification.Level(after), level)
}

// TestRecomputeSnapshotIsolationIsConsistent proves the mechanism issue #52
// finding 2 relies on. RecomputeAllProgress used to read allScoringEvents
// and existingProgressPairs as two independent pool queries under READ
// COMMITTED (this project's default): a write landing between them — a
// user's very first scored action for a (storage, user) pair, committing
// exactly in that gap — would make existingProgressPairs see a row
// allScoringEvents never had a chance to score, resetting it to zero.
// loadRecomputeSnapshot now runs both reads inside one REPEATABLE READ,
// read-only transaction instead, which is unexported and so not callable
// directly from this package — this test exercises the same isolation level
// and query shape it relies on: open a REPEATABLE READ, read-only
// transaction, run one query, commit a write from a second connection, then
// run a second query in the SAME transaction and confirm it still can't see
// the write. That guarantee is what makes loadRecomputeSnapshot race-free.
func TestRecomputeSnapshotIsolationIsConsistent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	snapshotTx, err := testPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	require.NoError(t, err)
	defer func() { _ = snapshotTx.Rollback(ctx) }()

	var before int
	require.NoError(t, snapshotTx.QueryRow(ctx,
		`SELECT count(*) FROM user_progress WHERE storage_id = $1 AND user_id = $2`,
		storageID, userID).Scan(&before))
	require.Equal(t, 0, before)

	// A concurrent write, committed on a separate connection — the gap
	// between the snapshot's two reads, where the race used to live.
	_, err = execTest(ctx,
		`INSERT INTO user_progress (storage_id, user_id, xp) VALUES ($1, $2, 100)`,
		storageID, userID)
	require.NoError(t, err)

	var after int
	require.NoError(t, snapshotTx.QueryRow(ctx,
		`SELECT count(*) FROM user_progress WHERE storage_id = $1 AND user_id = $2`,
		storageID, userID).Scan(&after))
	assert.Equal(t, 0, after,
		"a REPEATABLE READ transaction must not see a write committed after it began, even in a later statement")
}

// TestRecomputeAllProgressAppliesCoalescing is what actually distinguishes
// the authoritative recompute from the optimistic live path: two separate
// createBatch calls for the same product minutes apart must collapse into
// one contribution once recomputed, even though the live path (deliberately,
// see bumpForLedgerReason's doc comment) counted both.
func TestRecomputeAllProgressAppliesCoalescing(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Pasta"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	// Two batches of the same product, 10 minutes apart — well inside the
	// 2-hour coalescing window — bypassing the live path entirely so the test
	// controls the timestamps precisely.
	base := time.Now().Add(-time.Hour)
	for _, offset := range []time.Duration{0, 10 * time.Minute} {
		batchID := newUUID(t)
		_, err := execTest(ctx, `
			INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_source)
			VALUES ($1, $2, $3, 1, 'derived')`, batchID, product.ID, location.ID)
		require.NoError(t, err)
		_, err = execTest(ctx, `
			INSERT INTO inventory_logs (id, product_id, batch_id, change_qty, reason, created_by, timestamp)
			VALUES ($1, $2, $3, 1, 'vision_ingestion', $4, $5)`,
			newUUID(t), product.ID, batchID, userID, base.Add(offset))
		require.NoError(t, err)
	}

	_, err = s.RecomputeAllProgress(ctx)
	require.NoError(t, err)

	xp, _, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, gamification.XPLedgerContribution, xp, "two events 10 minutes apart must coalesce into one contribution")
}

// TestRecomputeAllProgressResetsAPairWithNoRemainingEvents: the cache row is
// left in place deliberately — deleting it would just be a different way of
// losing track of the pair — but a from-scratch recompute must still zero it
// out rather than leaving the stale total from before the deletion untouched.
// This is what actually makes "deleting a product removes its contribution
// on the next recompute" (docs/specs/51-gamification-scoring.md) true: the
// cascade alone only empties inventory_logs, and something still has to
// notice that a *previously scored* pair now has nothing left to justify its
// cached XP.
func TestRecomputeAllProgressResetsAPairWithNoRemainingEvents(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Temporary"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase, CreatedBy: &userID,
	})
	require.NoError(t, err)

	before, _, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	require.Greater(t, before, 0, "the live write must have cached some XP before the deletion")

	// The product's only inventory_logs row cascades away with it
	// (docs/specs/02-data-model.md) — user_progress is deliberately left
	// exactly as the live path last wrote it, stale XP and all, since nothing
	// about deleting a product runs a recompute on its own.
	_, deleteErr := s.DeleteProduct(ctx, storageID, product.ID)
	require.NoError(t, deleteErr)

	n, err := s.RecomputeAllProgress(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1, "a pair with a stale row but no events must still be counted as recomputed")

	xp, level, streak, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found, "the row is reset, not deleted")
	assert.Equal(t, 0, xp, "the stale XP from before the deletion must not survive a from-scratch recompute")
	assert.Equal(t, 1, level)
	assert.Equal(t, 0, streak)
}

// insertHolidayWeek writes a holiday_weeks row directly, bypassing
// SetHolidayWeeks' docs/specs/52-gamification-quests-and-ui.md rule that only
// the current or a future week may be *marked*. Once time has passed, a week
// that was validly marked while it was still current/future is now history —
// exactly the state these tests need to set up directly, since the write
// path itself no longer offers a way to create it after the fact.
func insertHolidayWeek(t *testing.T, ctx context.Context, userID uuid.UUID, week time.Time) {
	t.Helper()
	_, err := execTest(ctx, `
		INSERT INTO holiday_weeks (user_id, week_start) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		userID, week)
	require.NoError(t, err)
}

// insertLedgerEventAt writes one scoreable inventory_logs row (with its
// paired batch, per docs/specs/02-data-model.md) at an exact timestamp, for
// tests that need to control which week an event falls in without waiting
// for real time to pass.
func insertLedgerEventAt(t *testing.T, ctx context.Context, productID, locationID, userID uuid.UUID, at time.Time) {
	t.Helper()
	batchID := newUUID(t)
	_, err := execTest(ctx, `
		INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_source)
		VALUES ($1, $2, $3, 1, 'derived')`, batchID, productID, locationID)
	require.NoError(t, err)
	_, err = execTest(ctx, `
		INSERT INTO inventory_logs (id, product_id, batch_id, change_qty, reason, created_by, timestamp)
		VALUES ($1, $2, $3, 1, 'vision_ingestion', $4, $5)`,
		newUUID(t), productID, batchID, userID, at)
	require.NoError(t, err)
}

// mostRecentMonday matches store.mondayOf's own algorithm (Monday-start,
// UTC) independently, rather than importing the unexported function, so the
// test's weeks line up with what the implementation itself will compute.
func mostRecentMonday(t *testing.T, at time.Time) time.Time {
	t.Helper()
	at = at.UTC()
	offset := (int(at.Weekday()) + 6) % 7
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -offset)
}

// TestRecomputeAllProgressStreakCountsConsecutiveActiveWeeks is the ordinary
// case: activity every week up to last week, nothing yet this week (which
// has not finished), still counts as an unbroken streak.
func TestRecomputeAllProgressStreakCountsConsecutiveActiveWeeks(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Bread"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	thisWeek := mostRecentMonday(t, time.Now())
	for weeksAgo := 1; weeksAgo <= 3; weeksAgo++ {
		insertLedgerEventAt(t, ctx, product.ID, location.ID, userID, thisWeek.AddDate(0, 0, -7*weeksAgo).Add(12*time.Hour))
	}

	_, err = s.RecomputeAllProgress(ctx)
	require.NoError(t, err)

	_, _, streak, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, 3, streak, "three consecutive active weeks, with this week not yet over, is a streak of 3")
}

// TestRecomputeAllProgressStreakPausesOnAHolidayWeek: a week marked holiday
// must not break the streak, but it must not extend it either — only the
// active weeks on either side of it count.
func TestRecomputeAllProgressStreakPausesOnAHolidayWeek(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Bread"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	thisWeek := mostRecentMonday(t, time.Now())
	// Active 1 and 3 weeks ago; 2 weeks ago is a marked holiday with no
	// activity at all.
	insertLedgerEventAt(t, ctx, product.ID, location.ID, userID, thisWeek.AddDate(0, 0, -7).Add(12*time.Hour))
	insertLedgerEventAt(t, ctx, product.ID, location.ID, userID, thisWeek.AddDate(0, 0, -21).Add(12*time.Hour))
	insertHolidayWeek(t, ctx, userID, thisWeek.AddDate(0, 0, -14))

	_, err = s.RecomputeAllProgress(ctx)
	require.NoError(t, err)

	_, _, streak, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, 2, streak, "the holiday week pauses the streak rather than breaking it, but does not itself count")
}

// TestRecomputeAllProgressStreakDoesNotCountAWeekThatIsBothActiveAndHoliday
// covers docs/specs/52-gamification-quests-and-ui.md's rule precisely:
// "activity during a holiday week still earns full XP ... [but] does not
// count toward the streak". A week that has both a scored contribution and a
// holiday marker must still be a paused week, not an active one, so it must
// not raise streak_weeks above what the surrounding active weeks alone would
// give.
func TestRecomputeAllProgressStreakDoesNotCountAWeekThatIsBothActiveAndHoliday(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Bread"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	thisWeek := mostRecentMonday(t, time.Now())
	// Active 1 and 3 weeks ago; 2 weeks ago is BOTH marked holiday AND has a
	// scored contribution (logged from a hotel room, say) — per the spec this
	// must still not count toward the streak, and must not cancel the
	// holiday either.
	insertLedgerEventAt(t, ctx, product.ID, location.ID, userID, thisWeek.AddDate(0, 0, -7).Add(12*time.Hour))
	insertLedgerEventAt(t, ctx, product.ID, location.ID, userID, thisWeek.AddDate(0, 0, -14).Add(12*time.Hour))
	insertLedgerEventAt(t, ctx, product.ID, location.ID, userID, thisWeek.AddDate(0, 0, -21).Add(12*time.Hour))
	insertHolidayWeek(t, ctx, userID, thisWeek.AddDate(0, 0, -14))

	_, err = s.RecomputeAllProgress(ctx)
	require.NoError(t, err)

	_, _, streak, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, 2, streak,
		"a week that is both active and on holiday must not count toward the streak — only the two genuinely active weeks should")
}

// TestRecomputeAllProgressStreakBreaksOnARealGap: a week with neither
// activity nor a holiday marker ends the streak — only the weeks after the
// gap (working backward from now) count.
func TestRecomputeAllProgressStreakBreaksOnARealGap(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Bread"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	thisWeek := mostRecentMonday(t, time.Now())
	// Active last week; a genuine gap two weeks ago (no holiday marker);
	// active again three weeks ago. The old activity must not resurrect the
	// streak across a real, unpaused gap.
	insertLedgerEventAt(t, ctx, product.ID, location.ID, userID, thisWeek.AddDate(0, 0, -7).Add(12*time.Hour))
	insertLedgerEventAt(t, ctx, product.ID, location.ID, userID, thisWeek.AddDate(0, 0, -21).Add(12*time.Hour))

	_, err = s.RecomputeAllProgress(ctx)
	require.NoError(t, err)

	_, _, streak, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, 1, streak, "an unmarked gap breaks the streak; activity before the gap does not count")
}

// TestRecomputeAllProgressAppliesCorrectionSupersedesRuleEndToEnd is the
// should-fix from round 1: the pure Coalesce tests prove the 5-vs-3 rule in
// isolation, but nothing previously chained a real ai_correction contribution
// through the full ConfirmIngestion -> RecomputeAllProgress path to prove the
// two layers agree once the authoritative recompute runs.
func TestRecomputeAllProgressAppliesCorrectionSupersedesRuleEndToEnd(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	proposed, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Ketchup"})
	require.NoError(t, err)
	actual, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Barbecue Sauce"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, job.ID, exactMatchProposal(t, "0", proposed.ID)))
	_, err = s.ConfirmIngestion(ctx, storageID, job.ID, &userID, []store.IngestDecision{
		{RowID: "0", Accept: true, ProductID: &actual.ID, Quantity: 1, LocationID: &location.ID},
	})
	require.NoError(t, err)

	_, err = s.RecomputeAllProgress(ctx)
	require.NoError(t, err)

	xp, _, _, found := progressRow(t, ctx, storageID, userID)
	require.True(t, found)
	assert.Equal(t, gamification.XPAICorrection, xp, "recompute must also pay 5, not 3+5, for a corrected row")
}

// TestConfirmIngestionRollsBackContributionsWithEverythingElse verifies the
// "same transaction as the change itself" claim
// (docs/specs/51-gamification-scoring.md) rather than only asserting it in a
// comment: when a later row in the same confirm fails validation, an earlier
// row's ai_correction contribution — and its live XP bump — must not survive
// either, exactly like the batch that row would have created.
func TestConfirmIngestionRollsBackContributionsWithEverythingElse(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	proposed, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Ketchup"})
	require.NoError(t, err)
	actual, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Barbecue Sauce"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	// The stored proposal must list both rows, or matchRowIDs rejects the
	// decision list before row 0 is ever processed — which would make this
	// test pass on that check alone without ever exercising a mid-transaction
	// rollback at all.
	require.NoError(t, s.CompleteJob(ctx, job.ID, exactMatchProposalWithASecondRow(t, "0", proposed.ID, "1")))

	_, err = s.ConfirmIngestion(ctx, storageID, job.ID, &userID, []store.IngestDecision{
		// Row 0 would earn ai_correction if the transaction committed.
		{RowID: "0", Accept: true, ProductID: &actual.ID, Quantity: 1, LocationID: &location.ID},
		// Row 1 has no product and no new_product, which ConfirmIngestion
		// rejects during the per-row loop — failing the whole confirm after
		// row 0 has already been processed within the same transaction.
		{RowID: "1", Accept: true, Quantity: 1, LocationID: &location.ID},
	})
	require.Error(t, err)

	assert.Equal(t, 0, contributionCount(t, ctx, storageID, userID, gamification.KindAICorrection),
		"a failed confirm must roll back row 0's contribution along with its batch")
	_, _, _, found := progressRow(t, ctx, storageID, userID)
	assert.False(t, found, "and the XP it would have bumped")
}

// TestHealthScoreForStorage builds one product with every box checked and one
// with none, so the expected mean is exactly 50 on every sub-score.
func TestHealthScoreForStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)
	category, err := s.CreateCategory(ctx, storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)

	complete, err := s.CreateProduct(ctx, storageID, store.NewProduct{
		Name: "Complete", CategoryID: &category.ID, ItemType: store.ItemPerishable, MinStock: 2,
	})
	require.NoError(t, err)
	_, err = execTest(ctx, `UPDATE products SET image_url = 'https://example.com/x.jpg' WHERE id = $1`, complete.ID)
	require.NoError(t, err)
	when := time.Now().AddDate(0, 0, 5)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: complete.ID, LocationID: location.ID, Quantity: 1,
		ExpirationDate: &when, ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase, CreatedBy: &userID,
	})
	require.NoError(t, err)

	bare, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Bare", ItemType: store.ItemPerishable})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: bare.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	// No category, no image, no min_stock, and its only log row is backdated
	// past the 180-day staleness window. Its expiration_date is force-cleared
	// rather than left as CreateBatch left it: a perishable product with no
	// explicit date still gets a *derived* one by default
	// (docs/specs/08-expiration-and-classification.md), which would otherwise
	// make this fixture's "nothing filled in" batch carry a date anyway.
	_, err = execTest(ctx, `UPDATE inventory_batches SET expiration_date = NULL WHERE product_id = $1`, bare.ID)
	require.NoError(t, err)
	_, err = execTest(ctx, `UPDATE inventory_logs SET timestamp = now() - interval '200 days' WHERE product_id = $1`, bare.ID)
	require.NoError(t, err)

	score, err := s.HealthScoreForStorage(ctx, storageID)
	require.NoError(t, err)
	assert.InDelta(t, 50.0, score, 0.01)
}

func TestHealthScoreForStorageWithNoProductsIsZero(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	score, err := s.HealthScoreForStorage(ctx, storageID)
	require.NoError(t, err)
	assert.Equal(t, 0.0, score)
}

// TestLeaderboardOrdersByXPAndIncludesEveryMember: a member with no scored
// activity yet still appears, at zero — the leaderboard is a member list, not
// a list of people who have already contributed.
func TestLeaderboardOrdersByXPAndIncludesEveryMember(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	leader := newUser(t, ctx)
	quiet := newUser(t, ctx)
	_, err := execTest(ctx, `INSERT INTO storage_members (storage_id, user_id) VALUES ($1, $2), ($1, $3)`, storageID, leader, quiet)
	require.NoError(t, err)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Coffee"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase, CreatedBy: &leader,
	})
	require.NoError(t, err)

	entries, err := s.Leaderboard(ctx, storageID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, leader, entries[0].UserID, "the member with XP sorts first")
	assert.Equal(t, gamification.XPLedgerContribution, entries[0].XP)
	assert.Equal(t, quiet, entries[1].UserID)
	assert.Equal(t, 0, entries[1].XP)
}

func TestUserPreferencesDefaultToEnabledWithNoHolidays(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	prefs, err := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, err)
	assert.True(t, prefs.GamificationEnabled)
	assert.Empty(t, prefs.HolidayWeeks)
}

func TestSetGamificationEnabledPersists(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	require.NoError(t, s.SetGamificationEnabled(ctx, userID, false))
	prefs, err := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, err)
	assert.False(t, prefs.GamificationEnabled)

	require.NoError(t, s.SetGamificationEnabled(ctx, userID, true))
	prefs, err = s.UserPreferencesFor(ctx, userID)
	require.NoError(t, err)
	assert.True(t, prefs.GamificationEnabled)
}

// TestSetPreferencesIsAtomic is the regression for #54 finding 5.
// UpdateMePreferences used to call SetGamificationEnabled and
// SetHolidayWeeks as two separate store writes for what the UI presents as
// one PUT; a failure between them left the toggle changed and the holiday
// weeks untouched. SetPreferences now runs both in one transaction. Proving
// it needs no lock contention, unlike #35's tests: a deliberately invalid
// past week reliably fails setHolidayWeeksTx's own validation, so if the
// enabled-flag write shares its transaction, it must roll back too.
func TestSetPreferencesIsAtomic(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	require.NoError(t, s.SetGamificationEnabled(ctx, userID, true))

	pastWeek := time.Now().AddDate(0, 0, -14)
	err := s.SetPreferences(ctx, userID, false, []time.Time{pastWeek})
	require.Error(t, err, "a past week not already on record must be rejected")

	prefs, err := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, err)
	assert.True(t, prefs.GamificationEnabled,
		"the gamification-enabled write must have rolled back together with the failed holiday-weeks write")
}

// TestSetHolidayWeeksReplacesTheWholeSet: PUT semantics, not a patch — a
// second call with a different set must not merge with the first.
func TestSetHolidayWeeksReplacesTheWholeSet(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)
	thisWeek := mostRecentMonday(t, time.Now())
	first := thisWeek.AddDate(0, 0, 7)
	second := thisWeek.AddDate(0, 0, 70)

	require.NoError(t, s.SetHolidayWeeks(ctx, userID, []time.Time{first}))
	prefs, err := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, err)
	require.Len(t, prefs.HolidayWeeks, 1)

	require.NoError(t, s.SetHolidayWeeks(ctx, userID, []time.Time{second}))
	prefs, err = s.UserPreferencesFor(ctx, userID)
	require.NoError(t, err)
	require.Len(t, prefs.HolidayWeeks, 1)
	assert.True(t, prefs.HolidayWeeks[0].Equal(second))
}

// TestSetHolidayWeeksRejectsAPastWeek: "only the current week and future
// weeks may be marked" (docs/specs/52-gamification-quests-and-ui.md) — this
// is the budget-abuse door the rule closes, so a validation bypass here is a
// streak-recovery exploit, not a cosmetic bug.
func TestSetHolidayWeeksRejectsAPastWeek(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)
	pastWeek := mostRecentMonday(t, time.Now()).AddDate(0, 0, -7)

	err := s.SetHolidayWeeks(ctx, userID, []time.Time{pastWeek})

	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrValidation)

	prefs, prefsErr := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, prefsErr)
	assert.Empty(t, prefs.HolidayWeeks, "the rejected week must not be written")
}

// TestSetHolidayWeeksRejectsExceedingTheRollingBudget: at most 8 weeks in any
// rolling 52-week window, rejected with 409 stating how many remain
// (docs/specs/52-gamification-quests-and-ui.md).
func TestSetHolidayWeeksRejectsExceedingTheRollingBudget(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)
	thisWeek := mostRecentMonday(t, time.Now())

	eight := make([]time.Time, 8)
	for i := range eight {
		eight[i] = thisWeek.AddDate(0, 0, 7*(i+1))
	}
	require.NoError(t, s.SetHolidayWeeks(ctx, userID, eight), "exactly 8 weeks must be within budget")

	nine := append(append([]time.Time{}, eight...), thisWeek.AddDate(0, 0, 7*9))
	err := s.SetHolidayWeeks(ctx, userID, nine)

	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrConflict)
	assert.Contains(t, err.Error(), "0 week(s) remaining", "the 409 must state how many weeks remain in the window")

	prefs, prefsErr := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, prefsErr)
	assert.Len(t, prefs.HolidayWeeks, 8, "the rejected 9th week must not be written, and the first 8 must be untouched")
}

// TestSetHolidayWeeksUnmarkingAFutureWeekRefundsTheBudget is the positive
// half of the rule: "un-marking a future week refunds the budget"
// (docs/specs/52-gamification-quests-and-ui.md) — removing one of eight
// already-marked future weeks must free the slot for a new one, not leave
// the caller permanently capped at eight distinct weeks ever marked.
func TestSetHolidayWeeksUnmarkingAFutureWeekRefundsTheBudget(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)
	thisWeek := mostRecentMonday(t, time.Now())

	eight := make([]time.Time, 8)
	for i := range eight {
		eight[i] = thisWeek.AddDate(0, 0, 7*(i+1))
	}
	require.NoError(t, s.SetHolidayWeeks(ctx, userID, eight))

	// Drop the first of the eight, then add a brand-new ninth week. Without a
	// refund this would still be nine distinct weeks in one window and would
	// be rejected exactly like the sibling test above.
	sevenPlusOneNew := append(append([]time.Time{}, eight[1:]...), thisWeek.AddDate(0, 0, 7*9))
	err := s.SetHolidayWeeks(ctx, userID, sevenPlusOneNew)

	require.NoError(t, err, "un-marking one future week must refund its budget slot for the new one")

	prefs, prefsErr := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, prefsErr)
	assert.Len(t, prefs.HolidayWeeks, 8)
	weeks := map[time.Time]bool{}
	for _, w := range prefs.HolidayWeeks {
		weeks[w] = true
	}
	assert.False(t, weeks[eight[0]], "the un-marked week must actually be gone")
	assert.True(t, weeks[thisWeek.AddDate(0, 0, 7*9)], "the new ninth week must be present")
}

// TestSetHolidayWeeksNeverRemovesAPastOrCurrentWeek: a caller that only ever
// lists future weeks going forward must not be able to erase the record of
// an already-passed holiday week, which is what would let its budget slot be
// reused (docs/specs/52-gamification-quests-and-ui.md: "un-marking the
// current or a past week does not" refund the budget).
func TestSetHolidayWeeksNeverRemovesAPastOrCurrentWeek(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)
	thisWeek := mostRecentMonday(t, time.Now())
	pastWeek := thisWeek.AddDate(0, 0, -7)
	insertHolidayWeek(t, ctx, userID, pastWeek)
	insertHolidayWeek(t, ctx, userID, thisWeek)

	// A client resending only a future week, with no mention of the past or
	// current ones, must not erase them.
	future := thisWeek.AddDate(0, 0, 7)
	require.NoError(t, s.SetHolidayWeeks(ctx, userID, []time.Time{future}))

	prefs, err := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, err)
	weeks := map[time.Time]bool{}
	for _, w := range prefs.HolidayWeeks {
		weeks[w] = true
	}
	assert.True(t, weeks[pastWeek], "a past holiday week must survive a replace that does not mention it")
	assert.True(t, weeks[thisWeek], "the current week must survive a replace that does not mention it")
	assert.True(t, weeks[future], "the newly requested future week must be added")
}

// TestSetHolidayWeeksAcceptsResendingAnExistingPastWeek is the exact request
// shape web/static/js/pages/settings.js sends: GET /api/me/preferences
// returns full history, and adding or removing a future week naively resends
// that whole list. A past week already on record must not turn that into a
// validation failure — only a genuinely new attempt to backdate one should.
func TestSetHolidayWeeksAcceptsResendingAnExistingPastWeek(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)
	thisWeek := mostRecentMonday(t, time.Now())
	pastWeek := thisWeek.AddDate(0, 0, -7)
	insertHolidayWeek(t, ctx, userID, pastWeek)

	future := thisWeek.AddDate(0, 0, 7)
	err := s.SetHolidayWeeks(ctx, userID, []time.Time{pastWeek, future})

	require.NoError(t, err, "resending an already-recorded past week alongside a new future one must not be rejected")

	prefs, err := s.UserPreferencesFor(ctx, userID)
	require.NoError(t, err)
	weeks := map[time.Time]bool{}
	for _, w := range prefs.HolidayWeeks {
		weeks[w] = true
	}
	assert.True(t, weeks[pastWeek])
	assert.True(t, weeks[future])
}

func TestStorageGamificationSettingsDefaultToLeaderboardOff(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	settings, err := s.StorageGamificationSettingsFor(ctx, storageID)
	require.NoError(t, err)
	assert.False(t, settings.LeaderboardEnabled)
	assert.Equal(t, 20, settings.WeeklyGoalItems)
}

func TestUpdateStorageGamificationSettingsPersists(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	require.NoError(t, s.UpdateStorageGamificationSettings(ctx, storageID, true, 40))
	settings, err := s.StorageGamificationSettingsFor(ctx, storageID)
	require.NoError(t, err)
	assert.True(t, settings.LeaderboardEnabled)
	assert.Equal(t, 40, settings.WeeklyGoalItems)
}

func TestUpdateStorageGamificationSettingsRejectsNegativeGoal(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	err := s.UpdateStorageGamificationSettings(ctx, storageID, false, -1)
	assert.ErrorIs(t, err, store.ErrValidation)
}

// TestOverallProgressForUserAggregatesAcrossStorages: total_xp sums the
// caller's own storages, and overall_level is derived from that sum rather
// than being the sum of the per-storage levels.
func TestOverallProgressForUserAggregatesAcrossStorages(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	for _, name := range []string{"Home", "Cellar"} {
		storageID := newStorage(t, ctx)
		_, err := execTest(ctx, `INSERT INTO storage_members (storage_id, user_id) VALUES ($1, $2)`, storageID, userID)
		require.NoError(t, err)
		_, err = execTest(ctx, `UPDATE storages SET name = $1 WHERE id = $2`, name, storageID)
		require.NoError(t, err)

		product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "X"})
		require.NoError(t, err)
		location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Y"})
		require.NoError(t, err)
		_, err = s.CreateBatch(ctx, storageID, store.NewBatch{
			ProductID: product.ID, LocationID: location.ID, Quantity: 1, Reason: store.ReasonPurchase, CreatedBy: &userID,
		})
		require.NoError(t, err)
	}

	overall, err := s.OverallProgressForUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, 2*gamification.XPLedgerContribution, overall.TotalXP)
	assert.Equal(t, gamification.Level(overall.TotalXP), overall.OverallLevel)
	assert.NotEqual(t, gamification.Level(gamification.XPLedgerContribution)*2, overall.OverallLevel,
		"overall_level must not be the sum of the per-storage levels")
	assert.Len(t, overall.PerStorage, 2)
}

// TestUserProgressInStorageReturnsZeroValueNotError: "never contributed" is a
// valid state, not a missing one.
func TestUserProgressInStorageReturnsZeroValueNotError(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	p, err := s.UserProgressInStorage(ctx, storageID, userID)
	require.NoError(t, err)
	assert.Equal(t, 0, p.XP)
	assert.Equal(t, 1, p.Level)
}
