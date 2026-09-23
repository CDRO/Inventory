package store_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// barcodeSeq numbers the codes uniqueCode hands out.
var barcodeSeq atomic.Uint64

// uniqueCode returns a barcode no other test in this package uses.
//
// catalog_barcodes is global — it carries no storage_id, deliberately
// (docs/specs/02-data-model.md) — so two tests sharing a literal share a row,
// and one silently sees the hint the other wrote. Every other table here is
// storage-scoped and immune; this one is the exception, and the helper is what
// keeps these tests independent of each other's order.
func uniqueCode() string {
	return fmt.Sprintf("9%012d", barcodeSeq.Add(1))
}

// catalogued creates a product in storageID that carries a catalog_id, so that
// associating a code with it also writes the global hint.
func catalogued(t *testing.T, ctx context.Context, s *store.Store, storageID uuid.UUID, name string) (*store.Product, *store.CatalogProduct) {
	t.Helper()

	catalog, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{DisplayName: name})
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: name, CatalogID: &catalog.ID})
	require.NoError(t, err)
	return product, catalog
}

// This one is pure logic over literal inputs — no database, so no uniqueCode.
func TestValidateBarcodeAcceptsTheSpecCharsetAndRefusesTheRest(t *testing.T) {
	for _, code := range []string{"4006381333931", "abcXYZ", "a.b_c/d-e", strings.Repeat("9", 64)} {
		assert.NoError(t, store.ValidateBarcode(code), "should accept %q", code)
	}
	for _, code := range []string{"", "12 34", "1234\n", "Grüße", strings.Repeat("9", 65)} {
		assert.ErrorIs(t, store.ValidateBarcode(code), store.ErrValidation, "should refuse %q", code)
	}
}

// TestAssociateThenLookupResolvesLocallyWithNoCatalogFallback is the core
// promise of docs/specs/20-barcode-recall.md: the second scan of anything is
// free, and it resolves the storage's own product rather than the catalogue's
// description of it.
func TestAssociateThenLookupResolvesLocally(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	product, _ := catalogued(t, ctx, s, storageID, "Canned Tomatoes")

	association, err := s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)
	assert.Equal(t, product.ID, association.ProductID)

	hit, err := s.LookupBarcode(ctx, storageID, code)
	require.NoError(t, err)
	require.NotNil(t, hit.Product)
	assert.Nil(t, hit.Catalog, "a local hit must not also return the anonymous card")
	assert.Equal(t, product.ID, hit.Product.ID)
	assert.Equal(t, 0, hit.CurrentStock)
}

func TestLookupBarcodeReportsCurrentStock(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, locationID, _ := stocked(t, ctx, s, 3)
	_, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: productID, LocationID: locationID, Quantity: 2, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, storageID, productID, code)
	require.NoError(t, err)

	hit, err := s.LookupBarcode(ctx, storageID, code)
	require.NoError(t, err)
	require.NotNil(t, hit.Product)
	assert.Equal(t, 5, hit.CurrentStock)
}

// TestLookupUnknownBarcodeIsNotFound — an unknown code is indistinguishable
// from any other refusal (docs/specs/20-barcode-recall.md).
func TestLookupUnknownBarcodeIsNotFound(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	_, err := s.LookupBarcode(ctx, storageID, code)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// A code outside the stored charset is the same answer, not a complaint
	// about the code: the lookup has exactly one refusal.
	_, err = s.LookupBarcode(ctx, storageID, "not a barcode")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestTheSameBarcodeInTwoStoragesResolvesIndependently is the multi-tenancy
// criterion: storage A's association is invisible to storage B, beyond the
// anonymous catalogue card both may see.
func TestTheSameBarcodeInTwoStoragesResolvesIndependently(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	a := newStorage(t, ctx)
	b := newStorage(t, ctx)

	theirs, err := s.CreateProduct(ctx, a, store.NewProduct{Name: "Their Tomatoes"})
	require.NoError(t, err)
	ours, err := s.CreateProduct(ctx, b, store.NewProduct{Name: "Our Tomatoes"})
	require.NoError(t, err)

	_, err = s.AssociateBarcode(ctx, a, theirs.ID, code)
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, b, ours.ID, code)
	require.NoError(t, err, "the same physical code must be usable in both storages")

	hitA, err := s.LookupBarcode(ctx, a, code)
	require.NoError(t, err)
	require.NotNil(t, hitA.Product)
	assert.Equal(t, theirs.ID, hitA.Product.ID)

	hitB, err := s.LookupBarcode(ctx, b, code)
	require.NoError(t, err)
	require.NotNil(t, hitB.Product)
	assert.Equal(t, ours.ID, hitB.Product.ID)
}

// TestAssociatingAProductFromAnotherStorageIsNotFound — the same-storage rule
// on every id, answered as 404 rather than 403
// (docs/specs/03-auth-and-multi-tenancy.md).
func TestAssociatingAProductFromAnotherStorageIsNotFound(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	mine := newStorage(t, ctx)
	theirs := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, theirs, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)

	_, err = s.AssociateBarcode(ctx, mine, product.ID, code)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM product_barcodes WHERE product_id = $1`, product.ID))
}

// TestAssociatingATakenBarcodeIsAConflictAndDeletingFreesIt covers both halves
// of the acceptance criterion in one flow.
func TestAssociatingATakenBarcodeIsAConflictAndDeletingFreesIt(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	first, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Six-pack"})
	require.NoError(t, err)
	second, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Single"})
	require.NoError(t, err)

	_, err = s.AssociateBarcode(ctx, storageID, first.ID, code)
	require.NoError(t, err)

	_, err = s.AssociateBarcode(ctx, storageID, second.ID, code)
	assert.ErrorIs(t, err, store.ErrConflict)

	require.NoError(t, s.DeleteProductBarcode(ctx, storageID, first.ID, code))

	_, err = s.AssociateBarcode(ctx, storageID, second.ID, code)
	assert.NoError(t, err, "after the association is deleted the code is free again")
}

func TestDeletingAnUnknownAssociationIsNotFound(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)

	assert.ErrorIs(t, s.DeleteProductBarcode(ctx, storageID, product.ID, code), store.ErrNotFound)
}

func TestProductBarcodesIsStorageScopedAndEmptyNotNil(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	mine := newStorage(t, ctx)
	theirs := newStorage(t, ctx)

	ours, err := s.CreateProduct(ctx, mine, store.NewProduct{Name: "Ours"})
	require.NoError(t, err)
	list, err := s.ProductBarcodes(ctx, mine, ours.ID)
	require.NoError(t, err)
	assert.NotNil(t, list)
	assert.Empty(t, list)

	notOurs, err := s.CreateProduct(ctx, theirs, store.NewProduct{Name: "Theirs"})
	require.NoError(t, err)
	_, err = s.ProductBarcodes(ctx, mine, notOurs.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestACatalogOnlyBarcodeReturnsTheAnonymousCard — a household that has never
// seen the product gets the display-field card and nothing that could reveal
// another storage (docs/specs/02-data-model.md).
func TestACatalogOnlyBarcodeReturnsTheAnonymousCard(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	theirs := newStorage(t, ctx)
	mine := newStorage(t, ctx)

	product, catalog := catalogued(t, ctx, s, theirs, "Passata di Pomodoro")
	_, err := s.AssociateBarcode(ctx, theirs, product.ID, code)
	require.NoError(t, err)

	hit, err := s.LookupBarcode(ctx, mine, code)
	require.NoError(t, err)
	assert.Nil(t, hit.Product, "the other storage's product must never be returned")
	require.NotNil(t, hit.Catalog)
	assert.Equal(t, catalog.ID, hit.Catalog.ID, "the id stays server-side; the HTTP layer never serializes it")
	assert.Equal(t, "Passata di Pomodoro", hit.Catalog.DisplayName)
}

// TestCatalogBarcodesIsInsertOnly — the first mapping wins, exactly as
// catalog_products does, because a rewritable global row is a covert channel
// between households (docs/specs/02-data-model.md).
func TestCatalogBarcodesIsInsertOnly(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	first := newStorage(t, ctx)
	second := newStorage(t, ctx)

	firstProduct, firstCatalog := catalogued(t, ctx, s, first, "Real Tomatoes")
	_, err := s.AssociateBarcode(ctx, first, firstProduct.ID, code)
	require.NoError(t, err)

	secondProduct, _ := catalogued(t, ctx, s, second, "Something Else Entirely")
	_, err = s.AssociateBarcode(ctx, second, secondProduct.ID, code)
	require.NoError(t, err, "the local association still succeeds in the second storage")

	var catalogID uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT catalog_id FROM catalog_barcodes WHERE barcode = $1`, code).Scan(&catalogID))
	assert.Equal(t, firstCatalog.ID, catalogID, "the second claim must not overwrite the first")
}

// TestAssociatingAProductWithNoCatalogIDWritesNoHint — there is no anonymous
// description to point a global code at.
func TestAssociatingAProductWithNoCatalogIDWritesNoHint(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Homemade Jam"})
	require.NoError(t, err)

	_, err = s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)
	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM catalog_barcodes WHERE barcode = $1`, code))
}

// TestAdminDeleteOfACatalogBarcodeLeavesEveryLocalAssociationAlone is the
// acceptance criterion verbatim, and the reason the two tables are separate.
func TestAdminDeleteOfACatalogBarcodeLeavesEveryLocalAssociationAlone(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	admin := newUser(t, ctx)

	product, _ := catalogued(t, ctx, s, storageID, "Wrongly Named Thing")
	_, err := s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)
	require.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM catalog_barcodes WHERE barcode = $1`, code))

	require.NoError(t, s.DeleteCatalogBarcode(ctx, admin, code))

	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM catalog_barcodes WHERE barcode = $1`, code))
	assert.Equal(t, 1, countRows(t,
		ctx, `SELECT count(*) FROM product_barcodes WHERE storage_id = $1 AND barcode = $2`, storageID, code),
		"the household keeps the association it made")

	hit, err := s.LookupBarcode(ctx, storageID, code)
	require.NoError(t, err)
	assert.NotNil(t, hit.Product)

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM admin_audit_log WHERE action = 'catalog_barcode_deleted' AND target = $1`,
		code), "moderation crosses every household, so it is audited")
}

// TestAfterAdminDeletionTheNextAssociationMayReInsertTheHint — how a wrong
// first claim is corrected rather than frozen forever.
func TestAfterAdminDeletionTheNextAssociationMayReInsertTheHint(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	wrong := newStorage(t, ctx)
	right := newStorage(t, ctx)
	admin := newUser(t, ctx)

	wrongProduct, _ := catalogued(t, ctx, s, wrong, "Mislabelled")
	_, err := s.AssociateBarcode(ctx, wrong, wrongProduct.ID, code)
	require.NoError(t, err)
	require.NoError(t, s.DeleteCatalogBarcode(ctx, admin, code))

	rightProduct, rightCatalog := catalogued(t, ctx, s, right, "Correctly Named")
	_, err = s.AssociateBarcode(ctx, right, rightProduct.ID, code)
	require.NoError(t, err)

	var catalogID uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT catalog_id FROM catalog_barcodes WHERE barcode = $1`, code).Scan(&catalogID))
	assert.Equal(t, rightCatalog.ID, catalogID)
}

func TestDeletingAnUnknownCatalogBarcodeIsNotFoundAndWritesNoAudit(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	admin := newUser(t, ctx)

	before := countRows(t, ctx, `SELECT count(*) FROM admin_audit_log WHERE actor_id = $1`, admin)
	assert.ErrorIs(t, s.DeleteCatalogBarcode(ctx, admin, code), store.ErrNotFound)
	assert.Equal(t, before, countRows(t, ctx, `SELECT count(*) FROM admin_audit_log WHERE actor_id = $1`, admin))
}

// TestAssociatingABarcodeWritesNoGamificationContribution — accepting the
// capture-time offer is never scored, in any of its three outcomes
// (docs/specs/20-barcode-recall.md).
func TestAssociatingABarcodeWritesNoGamificationContribution(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	product, _ := catalogued(t, ctx, s, storageID, "Scored Nothing")

	before := countRows(t, ctx, `SELECT count(*) FROM contribution_events WHERE storage_id = $1`, storageID)
	_, err := s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)
	assert.Equal(t, before,
		countRows(t, ctx, `SELECT count(*) FROM contribution_events WHERE storage_id = $1`, storageID))
}

// TestMergeRepointsTheSourcesBarcodesToTheSurvivor is the acceptance
// criterion from docs/specs/16-product-maintenance.md's side of this feature.
//
// The "dropping any that collide" half of that criterion is unreachable
// through any path: product_barcodes' primary key is (storage_id, barcode), so
// two products in one storage cannot carry the same code in the first place —
// not through the API and not through raw SQL. repointBarcodes deletes
// colliding source rows before the update anyway, which is what makes the
// criterion true by construction rather than by luck; see its doc comment.
func TestMergeRepointsTheSourcesBarcodesToTheSurvivor(t *testing.T) {
	code1, code2, code3 := uniqueCode(), uniqueCode(), uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	survivor, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Tomatoes"})
	require.NoError(t, err)
	source, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Tomatos"})
	require.NoError(t, err)

	_, err = s.AssociateBarcode(ctx, storageID, survivor.ID, code1)
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, storageID, source.ID, code2)
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, storageID, source.ID, code3)
	require.NoError(t, err)

	// Another storage's identical code must survive untouched: the re-point is
	// scoped by storage_id, not by barcode alone.
	other := newStorage(t, ctx)
	elsewhere, err := s.CreateProduct(ctx, other, store.NewProduct{Name: "Elsewhere"})
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, other, elsewhere.ID, code2)
	require.NoError(t, err)

	_, err = s.MergeProducts(ctx, storageID, survivor.ID, source.ID)
	require.NoError(t, err)

	codes, err := s.ProductBarcodes(ctx, storageID, survivor.ID)
	require.NoError(t, err)
	got := make([]string, 0, len(codes))
	for _, c := range codes {
		got = append(got, c.Barcode)
	}
	assert.ElementsMatch(t, []string{code1, code2, code3}, got,
		"the survivor keeps its own code and gains the source's")

	// Every one of them now recalls the survivor.
	for _, code := range got {
		hit, err := s.LookupBarcode(ctx, storageID, code)
		require.NoError(t, err)
		require.NotNil(t, hit.Product)
		assert.Equal(t, survivor.ID, hit.Product.ID)
	}

	var owner uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT product_id FROM product_barcodes WHERE storage_id = $1 AND barcode = $2`,
		other, code2).Scan(&owner))
	assert.Equal(t, elsewhere.ID, owner, "the other storage's identical code is untouched")
}

// TestLogBarcodeInWritesABatchAndItsLogRow — the project's central invariant,
// checked on the newest write path (docs/specs/02-data-model.md).
func TestLogBarcodeInWritesABatchAndItsLogRow(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)

	before := logCount(t, ctx, product.ID)
	result, err := s.LogBarcode(ctx, storageID, code, &userID, store.BarcodeLogInput{
		Direction: store.BarcodeLogIn, Quantity: 4, LocationID: &location.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, result.CreatedBatchID)
	assert.Equal(t, 4, result.CurrentStock)
	assert.Equal(t, before+1, logCount(t, ctx, product.ID))
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM inventory_logs WHERE batch_id = $1 AND change_qty = 4 AND reason = 'purchase'`,
		*result.CreatedBatchID))
}

// TestLogBarcodeInLeavesAnUneditedDateDerived — the derived/user distinction
// the expiry cascade depends on (docs/specs/08-expiration-and-classification.md).
func TestLogBarcodeInLeavesAnUneditedDateDerived(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Milk"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)

	derived, err := s.LogBarcode(ctx, storageID, code, nil, store.BarcodeLogInput{
		Direction: store.BarcodeLogIn, Quantity: 1, LocationID: &location.ID,
	})
	require.NoError(t, err)

	edited, err := s.LogBarcode(ctx, storageID, code, nil, store.BarcodeLogInput{
		Direction: store.BarcodeLogIn, Quantity: 1, LocationID: &location.ID,
		ExpirationDate: day(2027, time.March, 1), ExpirationEdited: true,
	})
	require.NoError(t, err)

	var derivedSource, editedSource string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT expiration_source FROM inventory_batches WHERE id = $1`, *derived.CreatedBatchID).Scan(&derivedSource))
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT expiration_source FROM inventory_batches WHERE id = $1`, *edited.CreatedBatchID).Scan(&editedSource))
	assert.Equal(t, "derived", derivedSource)
	assert.Equal(t, "user", editedSource)
}

// TestLogBarcodeOutDecrementsNearestExpiryFirst — the same allocation the
// consumption review proposes (docs/specs/09-consumption-logging.md).
func TestLogBarcodeOutDecrementsNearestExpiryFirst(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Yoghurt"})
	require.NoError(t, err)
	location, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
	require.NoError(t, err)
	near, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 2,
		ExpirationDate: day(2027, time.January, 1), ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	far, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: product.ID, LocationID: location.ID, Quantity: 5,
		ExpirationDate: day(2027, time.June, 1), ExpirationSource: store.ExpirationUser, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)

	result, err := s.LogBarcode(ctx, storageID, code, nil, store.BarcodeLogInput{
		Direction: store.BarcodeLogOut, Quantity: 3,
	})
	require.NoError(t, err)
	assert.Equal(t, 4, result.CurrentStock)
	assert.ElementsMatch(t, []uuid.UUID{near.ID, far.ID}, result.TouchedBatchIDs)

	// The nearest batch is emptied and therefore deleted; the far one is
	// reduced by the remainder.
	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM inventory_batches WHERE id = $1`, near.ID))
	var remaining int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT quantity FROM inventory_batches WHERE id = $1`, far.ID).Scan(&remaining))
	assert.Equal(t, 4, remaining)
}

// TestLogBarcodeOutRefusesAnOverDecrement — clamping would record a
// consumption that did not happen (docs/specs/09-consumption-logging.md).
func TestLogBarcodeOutRefusesAnOverDecrement(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, _ := stocked(t, ctx, s, 2)
	_, err := s.AssociateBarcode(ctx, storageID, productID, code)
	require.NoError(t, err)

	before := logCount(t, ctx, productID)
	_, err = s.LogBarcode(ctx, storageID, code, nil, store.BarcodeLogInput{
		Direction: store.BarcodeLogOut, Quantity: 5,
	})
	assert.ErrorIs(t, err, store.ErrValidation)
	assert.Equal(t, before, logCount(t, ctx, productID), "a refused decrement writes nothing")
	assert.Equal(t, 2, stockOf(t, ctx, storageID, productID))
}

// TestLogBarcodeOutRefusesABatchOfAnotherProduct — same-storage validation is
// not enough on its own; the batch has to belong to the scanned product.
func TestLogBarcodeOutRefusesABatchOfAnotherProduct(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID, scannedID, locationID, _ := stocked(t, ctx, s, 2)

	other, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Something else"})
	require.NoError(t, err)
	otherBatch, err := s.CreateBatch(ctx, storageID, store.NewBatch{
		ProductID: other.ID, LocationID: locationID, Quantity: 5, Reason: store.ReasonPurchase,
	})
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, storageID, scannedID, code)
	require.NoError(t, err)

	_, err = s.LogBarcode(ctx, storageID, code, nil, store.BarcodeLogInput{
		Direction: store.BarcodeLogOut, Quantity: 1,
		Decrements: []store.ConsumeBatchDecrement{{BatchID: otherBatch.ID, Quantity: 1}},
	})
	assert.ErrorIs(t, err, store.ErrValidation)
	assert.Equal(t, 5, stockOf(t, ctx, storageID, other.ID), "the other product's stock is untouched")
}

// TestLogBarcodeRefusesACatalogOnlyCode — the quick flow logs against a
// product this storage already has; a catalogue hit is an offer to create one.
func TestLogBarcodeRefusesACatalogOnlyCode(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	theirs := newStorage(t, ctx)
	mine := newStorage(t, ctx)

	product, _ := catalogued(t, ctx, s, theirs, "Their Beans")
	_, err := s.AssociateBarcode(ctx, theirs, product.ID, code)
	require.NoError(t, err)

	_, err = s.LogBarcode(ctx, mine, code, nil, store.BarcodeLogInput{
		Direction: store.BarcodeLogOut, Quantity: 1,
	})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestLogBarcodeRefusesAnUnknownDirectionAndAZeroQuantity(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, _ := stocked(t, ctx, s, 2)
	_, err := s.AssociateBarcode(ctx, storageID, productID, code)
	require.NoError(t, err)

	_, err = s.LogBarcode(ctx, storageID, code, nil, store.BarcodeLogInput{
		Direction: "sideways", Quantity: 1,
	})
	assert.ErrorIs(t, err, store.ErrValidation)

	_, err = s.LogBarcode(ctx, storageID, code, nil, store.BarcodeLogInput{
		Direction: store.BarcodeLogIn, Quantity: 0,
	})
	assert.ErrorIs(t, err, store.ErrValidation)
}

// TestBarcodePromptIsShownOnceWithTheFirstTimeCopy — "set exactly once, on the
// first time the offer is shown to a user, and never again".
func TestBarcodePromptIsShownOnceWithTheFirstTimeCopy(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	state, err := s.BarcodePromptState(ctx, userID)
	require.NoError(t, err)
	assert.True(t, state.Enabled, "the offer is on by default")
	assert.True(t, state.FirstTime)

	first, err := s.MarkBarcodePromptShown(ctx, userID)
	require.NoError(t, err)
	assert.True(t, first.FirstTime)

	second, err := s.MarkBarcodePromptShown(ctx, userID)
	require.NoError(t, err)
	assert.False(t, second.FirstTime, "the playful copy is never shown twice")

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM users WHERE id = $1 AND barcode_prompt_seen_at IS NOT NULL`, userID))
}

// TestTurningTheBarcodePromptOffStopsItEverywhereAndItStaysSeen — "the offer
// never appears again for that user, in any storage, until they re-enable it".
func TestTurningTheBarcodePromptOffStopsItEverywhere(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	shown, err := s.MarkBarcodePromptShown(ctx, userID)
	require.NoError(t, err)
	require.True(t, shown.FirstTime)

	off, err := s.SetBarcodePromptEnabled(ctx, userID, false)
	require.NoError(t, err)
	assert.False(t, off.Enabled)

	again, err := s.MarkBarcodePromptShown(ctx, userID)
	require.NoError(t, err)
	assert.False(t, again.Enabled, "a disabled offer is never shown")

	back, err := s.SetBarcodePromptEnabled(ctx, userID, true)
	require.NoError(t, err)
	assert.True(t, back.Enabled)
	assert.False(t, back.FirstTime, "re-enabling does not reset the first-time copy")
}

// TestMarkBarcodePromptShownDoesNotRecordADisabledOffer — nothing was shown,
// so there is nothing to remember having shown.
func TestMarkBarcodePromptShownDoesNotRecordADisabledOffer(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	_, err := s.SetBarcodePromptEnabled(ctx, userID, false)
	require.NoError(t, err)
	state, err := s.MarkBarcodePromptShown(ctx, userID)
	require.NoError(t, err)
	assert.False(t, state.Enabled)
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM users WHERE id = $1 AND barcode_prompt_seen_at IS NULL`, userID))

	back, err := s.SetBarcodePromptEnabled(ctx, userID, true)
	require.NoError(t, err)
	assert.True(t, back.FirstTime, "somebody who never saw the offer still gets the explanation")
}

func TestBarcodePromptStateForAnUnknownUserIsNotFound(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, err := s.BarcodePromptState(ctx, uuid.New())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.SetBarcodePromptEnabled(ctx, uuid.New(), false)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.MarkBarcodePromptShown(ctx, uuid.New())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestAcceptingACatalogHintCreatesTheProductAndAttachesTheCode — the
// server-side half of "accepting it creates the storage-local product the same
// way a shopping-list catalog card does, then associates the scanned code".
func TestAcceptingACatalogHintCreatesTheProductAndAttachesTheCode(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	theirs := newStorage(t, ctx)
	mine := newStorage(t, ctx)

	// Somebody else claimed the code for a catalogue row first; this storage
	// has never seen the product.
	source, catalog := catalogued(t, ctx, s, theirs, "Passata di Pomodoro")
	_, err := s.AssociateBarcode(ctx, theirs, source.ID, code)
	require.NoError(t, err)

	created, err := s.CreateProductFromBarcodeHint(ctx, mine, code)
	require.NoError(t, err)
	assert.Equal(t, "Passata di Pomodoro", created.Name)
	assert.Equal(t, mine, created.StorageID)
	require.NotNil(t, created.CatalogID)
	assert.Equal(t, catalog.ID, *created.CatalogID, "the product is linked to the catalogue row, like a list accept")
	assert.Nil(t, created.ImageURL, "the catalogue's provider URL is never copied onto a product")

	// The code now recalls the new product locally, and the other storage is
	// entirely unaffected.
	hit, err := s.LookupBarcode(ctx, mine, code)
	require.NoError(t, err)
	require.NotNil(t, hit.Product)
	assert.Equal(t, created.ID, hit.Product.ID)

	theirHit, err := s.LookupBarcode(ctx, theirs, code)
	require.NoError(t, err)
	require.NotNil(t, theirHit.Product)
	assert.Equal(t, source.ID, theirHit.Product.ID)
}

func TestAcceptingACatalogHintRefusesACodeThatAlreadyResolvesHere(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, _ := catalogued(t, ctx, s, storageID, "Already Mine")
	_, err := s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)

	_, err = s.CreateProductFromBarcodeHint(ctx, storageID, code)
	assert.ErrorIs(t, err, store.ErrConflict)
}

func TestAcceptingACodeWithNoCatalogHintIsNotFound(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()

	_, err := s.CreateProductFromBarcodeHint(ctx, newStorage(t, ctx), code)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestAcceptingACatalogHintWritesNoGamificationContribution — creating a
// product this way is not scored, exactly as the shopping-list catalogue
// accept it mirrors is not (that path pays only for resolving an ambiguity,
// which a barcode has none of).
func TestAcceptingACatalogHintWritesNoGamificationContribution(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	theirs := newStorage(t, ctx)
	mine := newStorage(t, ctx)

	source, _ := catalogued(t, ctx, s, theirs, "Unscored Beans")
	_, err := s.AssociateBarcode(ctx, theirs, source.ID, code)
	require.NoError(t, err)

	before := countRows(t, ctx, `SELECT count(*) FROM contribution_events WHERE storage_id = $1`, mine)
	_, err = s.CreateProductFromBarcodeHint(ctx, mine, code)
	require.NoError(t, err)
	assert.Equal(t, before,
		countRows(t, ctx, `SELECT count(*) FROM contribution_events WHERE storage_id = $1`, mine))
}

// TestDeletingAProductTakesItsBarcodesWithIt — ON DELETE CASCADE, so a code is
// free to be reused the moment the product it named is gone.
func TestDeletingAProductTakesItsBarcodesWithIt(t *testing.T) {
	code := uniqueCode()
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	product, err := s.CreateProduct(ctx, storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)
	_, err = s.AssociateBarcode(ctx, storageID, product.ID, code)
	require.NoError(t, err)

	_, err = s.DeleteProduct(ctx, storageID, product.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM product_barcodes WHERE storage_id = $1 AND barcode = $2`, storageID, code))
}

// stockOf is the live sum of a product's batches, read straight from the
// table rather than through the store's own reader.
func stockOf(t *testing.T, ctx context.Context, storageID, productID uuid.UUID) int {
	t.Helper()

	var total int
	require.NoError(t, testPool.QueryRow(ctx, `
		SELECT coalesce(sum(b.quantity), 0)
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE b.product_id = $1 AND p.storage_id = $2`, productID, storageID).Scan(&total))
	return total
}
