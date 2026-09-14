package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
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

	_, err = s.MoveBatch(ctx, storageID, batchID, kitchen.ID, &userID)
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

	_, err = s.ResolveShoppingListItem(ctx, storageID, items[0].ID, nil, 1, &userID)
	require.NoError(t, err)
	assert.Equal(t, 1, contributionCount(t, ctx, storageID, userID, gamification.KindAmbiguityResolved))

	_, err = s.ResolveShoppingListItem(ctx, storageID, items[1].ID, nil, 1, &userID)
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

// TestRecomputeAllProgressIgnoresDeletedProducts: inventory_logs.product_id
// cascades with the product it names, so a deleted product's history is
// simply absent from the next recompute — the "create-delete cycles earn
// nothing" rule requires no extra bookkeeping beyond that cascade.
func TestRecomputeAllProgressIgnoresDeletedProducts(t *testing.T) {
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

	require.NoError(t, s.DeleteProduct(ctx, storageID, product.ID))
	_, err = testPool.Exec(ctx, `DELETE FROM user_progress WHERE storage_id = $1`, storageID)
	require.NoError(t, err)

	_, err = s.RecomputeAllProgress(ctx)
	require.NoError(t, err)

	_, _, _, found := progressRow(t, ctx, storageID, userID)
	assert.False(t, found, "a deleted product's only contribution must not survive a from-scratch recompute")
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

// TestSetHolidayWeeksReplacesTheWholeSet: PUT semantics, not a patch — a
// second call with a different set must not merge with the first.
func TestSetHolidayWeeksReplacesTheWholeSet(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)
	first := time.Date(2026, time.January, 5, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, time.July, 6, 0, 0, 0, 0, time.UTC)

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
