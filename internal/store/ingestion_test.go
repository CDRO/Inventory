package store_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// ingestFixture is a storage with a shelf, a category, a product, and a done
// job whose proposal has the given number of rows.
type ingestFixture struct {
	storageID  uuid.UUID
	userID     uuid.UUID
	pantry     uuid.UUID
	dairy      uuid.UUID
	beans      uuid.UUID
	jobID      uuid.UUID
	rowIDs     []string
	logsBefore int
}

func newIngestFixture(t *testing.T, s *store.Store, rows int) ingestFixture {
	t.Helper()
	ctx := context.Background()

	f := ingestFixture{storageID: newStorage(t, ctx), userID: newUser(t, ctx)}

	pantry, err := s.CreateLocation(ctx, f.storageID, store.NewLocation{Name: "Pantry"})
	require.NoError(t, err)
	f.pantry = pantry.ID

	food, err := s.CreateCategory(ctx, f.storageID, store.NewCategory{Name: "Food"})
	require.NoError(t, err)
	dairy, err := s.CreateCategory(ctx, f.storageID, store.NewCategory{Name: "Dairy", ParentID: &food.ID})
	require.NoError(t, err)
	f.dairy = dairy.ID

	beans, err := s.CreateProduct(ctx, f.storageID, store.NewProduct{Name: "Beans"})
	require.NoError(t, err)
	f.beans = beans.ID

	type row struct {
		RowID string `json:"row_id"`
	}
	proposal := struct {
		Rows []row `json:"rows"`
	}{Rows: []row{}}
	for i := range rows {
		f.rowIDs = append(f.rowIDs, strconv.Itoa(i))
		proposal.Rows = append(proposal.Rows, row{RowID: strconv.Itoa(i)})
	}
	payload, err := json.Marshal(proposal)
	require.NoError(t, err)

	job, err := s.CreateJob(ctx, store.NewJob{StorageID: f.storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, job.ID, payload))
	f.jobID = job.ID

	f.logsBefore = countRows(t, ctx, `SELECT count(*) FROM inventory_logs l JOIN products p ON p.id = l.product_id WHERE p.storage_id = $1`, f.storageID)
	return f
}

func (f ingestFixture) batchCount(t *testing.T) int {
	t.Helper()
	return countRows(t, context.Background(), `
		SELECT count(*) FROM inventory_batches b JOIN products p ON p.id = b.product_id WHERE p.storage_id = $1`, f.storageID)
}

func TestConfirmIngestionAppliesEveryDecisionInOneGo(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newIngestFixture(t, s, 4)
	edited := time.Date(2027, time.March, 1, 0, 0, 0, 0, time.UTC)

	result, err := s.ConfirmIngestion(ctx, f.storageID, f.jobID, &f.userID, []store.IngestDecision{
		// An existing product on an existing shelf, default expiry accepted.
		{RowID: "0", Accept: true, ProductID: &f.beans, Quantity: 3, LocationID: &f.pantry},
		// A product the reviewer typed, on a shelf the model proposed.
		{RowID: "1", Accept: true, Quantity: 2,
			NewProduct:       &store.NewIngestProduct{Name: "Grandma's Chutney", CategoryID: &f.dairy, ItemType: store.ItemPerishable},
			NewLocation:      &store.NewIngestLocation{ParentID: &f.pantry, Names: []string{"Top Shelf", "Left"}},
			ExpirationEdited: true, ExpirationDate: &edited},
		// The same new product and path again: reused, not duplicated.
		{RowID: "2", Accept: true, Quantity: 1,
			NewProduct:  &store.NewIngestProduct{Name: "grandma's chutney", ItemType: store.ItemPerishable},
			NewLocation: &store.NewIngestLocation{ParentID: &f.pantry, Names: []string{"top shelf", "Left"}}},
		// A false positive.
		{RowID: "3", Accept: false},
	})
	require.NoError(t, err)

	assert.Len(t, result.BatchIDs, 3, "one batch per accepted row, none for the rejection")
	assert.Equal(t, 1, result.ProductsCreated, "the same new product named twice is created once")
	assert.Equal(t, 2, result.LocationsCreated, "Top Shelf and Left, once each")
	assert.Equal(t, 3, f.batchCount(t))

	assert.Equal(t, f.logsBefore+3, countRows(t, ctx, `
		SELECT count(*) FROM inventory_logs l JOIN products p ON p.id = l.product_id
		 WHERE p.storage_id = $1 AND l.reason = 'vision_ingestion' AND l.created_by = $2`, f.storageID, f.userID),
		"every batch is paired with a vision_ingestion log row")

	// Expiry source follows whether the reviewer touched the date.
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM inventory_batches WHERE id = $1 AND expiration_source = 'derived'`, result.BatchIDs[0]))
	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM inventory_batches WHERE id = $1 AND expiration_source = 'user' AND expiration_date = '2027-03-01'`, result.BatchIDs[1]))

	// The new product reached the catalog as a name and a category path, and
	// nothing else — no image, no storage.
	var catalogPath *string
	var imageURL *string
	require.NoError(t, testPool.QueryRow(ctx, `
		SELECT c.category_path, c.image_url FROM products p JOIN catalog_products c ON c.id = p.catalog_id
		 WHERE p.storage_id = $1 AND p.name = $2`, f.storageID, "Grandma's Chutney").Scan(&catalogPath, &imageURL))
	require.NotNil(t, catalogPath)
	assert.Equal(t, "Food > Dairy", *catalogPath)
	assert.Nil(t, imageURL, "a user's photo is never published to the catalog")

	assert.Equal(t, "consumed", jobStatus(t, ctx, f.jobID))

	// A second confirm — a double submit, or another member reviewing at the
	// same time — applies nothing.
	_, err = s.ConfirmIngestion(ctx, f.storageID, f.jobID, &f.userID, []store.IngestDecision{
		{RowID: "0", Accept: true, ProductID: &f.beans, Quantity: 3, LocationID: &f.pantry},
		{RowID: "1", Accept: false}, {RowID: "2", Accept: false}, {RowID: "3", Accept: false},
	})
	assert.ErrorIs(t, err, store.ErrConflict)
	assert.Equal(t, 3, f.batchCount(t))
}

// TestConfirmIngestionGivesANewProductItsPictureButNeverTheCatalog — a picture
// cut from the reviewed photo is the storage's own. It lands on the product,
// never on the anonymous catalog row, and only the storage whose product uses
// it can be told it exists.
func TestConfirmIngestionGivesANewProductItsPictureButNeverTheCatalog(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newIngestFixture(t, s, 2)

	first := "/api/storages/" + f.storageID.String() + "/product-images/" + uuid.Must(uuid.NewV7()).String() + ".jpg"
	second := "/api/storages/" + f.storageID.String() + "/product-images/" + uuid.Must(uuid.NewV7()).String() + ".jpg"

	_, err := s.ConfirmIngestion(ctx, f.storageID, f.jobID, &f.userID, []store.IngestDecision{
		{RowID: "0", Accept: true, Quantity: 1, LocationID: &f.pantry,
			NewProduct: &store.NewIngestProduct{Name: "Quince Jam", ImageURL: &first}},
		// The same new product again, with a different picture: the product is
		// created once, and keeps the first row's.
		{RowID: "1", Accept: true, Quantity: 1, LocationID: &f.pantry,
			NewProduct: &store.NewIngestProduct{Name: "quince jam", ImageURL: &second}},
	})
	require.NoError(t, err)

	var productImage, catalogImage *string
	require.NoError(t, testPool.QueryRow(ctx, `
		SELECT p.image_url, c.image_url FROM products p JOIN catalog_products c ON c.id = p.catalog_id
		 WHERE p.storage_id = $1 AND p.name = $2`, f.storageID, "Quince Jam").Scan(&productImage, &catalogImage))
	require.NotNil(t, productImage)
	assert.Equal(t, first, *productImage)
	assert.Nil(t, catalogImage, "a user's photo is never published to the catalog")

	used, err := s.ProductImageInStorage(ctx, f.storageID, first)
	require.NoError(t, err)
	assert.True(t, used, "the storage whose product uses the picture")

	used, err = s.ProductImageInStorage(ctx, f.storageID, second)
	require.NoError(t, err)
	assert.False(t, used, "a picture written for a row whose product was folded into another's")

	used, err = s.ProductImageInStorage(ctx, newStorage(t, ctx), first)
	require.NoError(t, err)
	assert.False(t, used, "any other storage, even asking for the exact URL")
}

// TestConfirmIngestionRequiresExactlyTheIssuedRows — a missing row is not an
// implicit rejection, and an unknown or doubled row is a client bug.
func TestConfirmIngestionRequiresExactlyTheIssuedRows(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newIngestFixture(t, s, 2)

	accept := func(id string) store.IngestDecision {
		return store.IngestDecision{RowID: id, Accept: true, ProductID: &f.beans, Quantity: 1, LocationID: &f.pantry}
	}

	for name, decisions := range map[string][]store.IngestDecision{
		"missing row": {accept("0")},
		"unknown row": {accept("0"), accept("1"), accept("7")},
		"doubled row": {accept("0"), accept("0")},
		"none at all": {},
	} {
		_, err := s.ConfirmIngestion(ctx, f.storageID, f.jobID, &f.userID, decisions)
		assert.ErrorIs(t, err, store.ErrValidation, name)
	}

	assert.Zero(t, f.batchCount(t), "a refused confirm writes nothing")
	assert.Equal(t, "done", jobStatus(t, ctx, f.jobID), "and leaves the job waiting for review")
}

// TestConfirmIngestionRollsBackOnAForeignId — every id in the body is checked
// against the storage, and a refusal late in the list undoes the rows before
// it: a product created for row 0 must not survive row 1's refusal.
func TestConfirmIngestionRollsBackOnAForeignId(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newIngestFixture(t, s, 2)
	other := newIngestFixture(t, s, 0)

	productsBefore := countRows(t, ctx, `SELECT count(*) FROM products WHERE storage_id = $1`, f.storageID)

	for name, second := range map[string]store.IngestDecision{
		"foreign location":        {RowID: "1", Accept: true, ProductID: &f.beans, Quantity: 1, LocationID: &other.pantry},
		"foreign product":         {RowID: "1", Accept: true, ProductID: &other.beans, Quantity: 1, LocationID: &f.pantry},
		"foreign location parent": {RowID: "1", Accept: true, ProductID: &f.beans, Quantity: 1, NewLocation: &store.NewIngestLocation{ParentID: &other.pantry, Names: []string{"Sneaky"}}},
		"foreign category":        {RowID: "1", Accept: true, Quantity: 1, LocationID: &f.pantry, NewProduct: &store.NewIngestProduct{Name: "Other Jam", CategoryID: &other.dairy}},
	} {
		_, err := s.ConfirmIngestion(ctx, f.storageID, f.jobID, &f.userID, []store.IngestDecision{
			{RowID: "0", Accept: true, Quantity: 1, LocationID: &f.pantry,
				NewProduct: &store.NewIngestProduct{Name: "Created Then Rolled Back " + name}},
			second,
		})
		assert.ErrorIs(t, err, store.ErrNotFound, name)
	}

	assert.Zero(t, f.batchCount(t))
	assert.Equal(t, productsBefore, countRows(t, ctx, `SELECT count(*) FROM products WHERE storage_id = $1`, f.storageID),
		"row 0's new product is rolled back with the refusal")
	assert.Zero(t, countRows(t, ctx, `SELECT count(*) FROM locations WHERE name = 'Sneaky'`))
	assert.Equal(t, "done", jobStatus(t, ctx, f.jobID))

	_, err := s.ConfirmIngestion(ctx, other.storageID, f.jobID, &f.userID, nil)
	assert.ErrorIs(t, err, store.ErrNotFound, "another storage cannot confirm this job")
}

// TestConfirmingAnEmptyProposalOnlyConsumesIt — the acceptance criterion: a
// no-op on inventory that still leaves the inbox.
func TestConfirmingAnEmptyProposalOnlyConsumesIt(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newIngestFixture(t, s, 0)

	result, err := s.ConfirmIngestion(ctx, f.storageID, f.jobID, &f.userID, nil)
	require.NoError(t, err)
	assert.Empty(t, result.BatchIDs)
	assert.Zero(t, f.batchCount(t))
	assert.Equal(t, "consumed", jobStatus(t, ctx, f.jobID))
}

// TestReingestingAShelfAddsBatches — a second photo of the same shelf never
// overwrites the stock the first one recorded.
func TestReingestingAShelfAddsBatches(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	f := newIngestFixture(t, s, 1)

	decisions := []store.IngestDecision{{RowID: "0", Accept: true, ProductID: &f.beans, Quantity: 2, LocationID: &f.pantry}}
	_, err := s.ConfirmIngestion(ctx, f.storageID, f.jobID, &f.userID, decisions)
	require.NoError(t, err)

	second, err := s.CreateJob(ctx, store.NewJob{StorageID: f.storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, second.ID, json.RawMessage(`{"rows":[{"row_id":"0"}]}`)))
	_, err = s.ConfirmIngestion(ctx, f.storageID, second.ID, &f.userID, decisions)
	require.NoError(t, err)

	assert.Equal(t, 2, f.batchCount(t))
	stock, err := s.CurrentStock(ctx, f.storageID, f.beans)
	require.NoError(t, err)
	assert.Equal(t, 4, stock)
}

func TestOnlyADoneJobCanBeConfirmed(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	pending, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	_, err = s.ConfirmIngestion(ctx, storageID, pending.ID, nil, nil)
	assert.ErrorIs(t, err, store.ErrConflict)

	failed, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	require.NoError(t, s.FailJob(ctx, failed.ID, "nope"))
	_, err = s.ConfirmIngestion(ctx, storageID, failed.ID, nil, nil)
	assert.ErrorIs(t, err, store.ErrConflict)
}

func TestJobPhotoAndHint(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	shelf, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Shelf"})
	require.NoError(t, err)
	foreign, err := s.CreateLocation(ctx, newStorage(t, ctx), store.NewLocation{Name: "Theirs"})
	require.NoError(t, err)

	name := "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.jpg"
	job, err := s.CreateJob(ctx, store.NewJob{
		StorageID: storageID, Kind: store.JobShelfIngestion, ImageFilename: &name, LocationHintID: &shelf.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, job.ImageFilename)
	assert.Equal(t, name, *job.ImageFilename)
	assert.Equal(t, shelf.ID, *job.LocationHintID)

	_, err = s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion, LocationHintID: &foreign.ID})
	assert.ErrorIs(t, err, store.ErrNotFound, "a hint in another storage is a 404 like any other foreign id")

	image, err := s.DeleteJob(ctx, storageID, job.ID)
	require.NoError(t, err)
	require.NotNil(t, image, "discarding hands back the photo so it can be deleted too")
	assert.Equal(t, name, *image)
}

// TestExpiredJobImagesAreOnlyReviewedOnes — a photo still waiting for review
// is never a candidate, however old; a consumed one is, once its grace ends.
func TestExpiredJobImagesAreOnlyReviewedOnes(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	newJob := func(name string) uuid.UUID {
		job, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion, ImageFilename: &name})
		require.NoError(t, err)
		require.NoError(t, s.CompleteJob(ctx, job.ID, json.RawMessage(`{"rows":[]}`)))
		return job.ID
	}
	waiting := newJob("0190a1b2-c3d4-7e5f-8a6b-000000000001.jpg")
	reviewed := newJob("0190a1b2-c3d4-7e5f-8a6b-000000000002.jpg")
	_, err := s.ConfirmIngestion(ctx, storageID, reviewed, nil, nil)
	require.NoError(t, err)
	_, err = execTest(ctx, `UPDATE jobs SET updated_at = now() - interval '40 days' WHERE id = ANY($1)`, []uuid.UUID{waiting, reviewed})
	require.NoError(t, err)

	expired, err := s.ExpiredJobImages(ctx, time.Now().Add(-30*24*time.Hour), 100)
	require.NoError(t, err)
	assert.Contains(t, expired, reviewed)
	assert.NotContains(t, expired, waiting, "an unreviewed proposal keeps its photo indefinitely")

	require.NoError(t, s.ClearJobImage(ctx, reviewed))
	expired, err = s.ExpiredJobImages(ctx, time.Now().Add(-30*24*time.Hour), 100)
	require.NoError(t, err)
	assert.NotContains(t, expired, reviewed)
}
