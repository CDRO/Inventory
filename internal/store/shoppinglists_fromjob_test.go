package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// CreateShoppingListFromJob is the reclassification of
// docs/specs/41-mixed-photo-classification.md: a photo somebody uploaded as a
// shelf, product or consumption photo becomes the shopping list it turned out
// to be, and the job that held it is discarded — in one transaction.

// doneJobWithPhoto seeds a job waiting for review, with a photo, and returns
// it.
func doneJobWithPhoto(t *testing.T, ctx context.Context, s *store.Store, storageID uuid.UUID) *store.Job {
	t.Helper()
	name := uuid.New().String() + ".jpg"
	job, err := s.CreateJob(ctx, store.NewJob{
		StorageID: storageID, Kind: store.JobShelfIngestion, ImageFilename: &name,
	})
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, job.ID, json.RawMessage(
		`{"mode":"shelf","rows":[],"looks_like_shopping_list":true,"shopping_list_lines":["milk"]}`)))
	return job
}

func TestReclassifyingWritesTheListAndDiscardsTheJobTogether(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	job := doneJobWithPhoto(t, ctx, s, storageID)

	list, items, image, err := s.CreateShoppingListFromJob(ctx, storageID, job.ID, nil, lines("milk", "eggs"))
	require.NoError(t, err)

	assert.Equal(t, store.SourcePhoto, list.Source, "a reclassified photo list is a photo list")
	require.Len(t, items, 2)
	require.NotNil(t, image)
	assert.Equal(t, *job.ImageFilename, *image, "the caller needs the filename to remove the photo after the commit")

	// Discarding means the row is gone, which is what "discarded" means in
	// docs/specs/06-vision-shelf-ingestion.md — there is no such job status.
	_, err = s.Job(ctx, storageID, job.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 2, countRows(t, ctx,
		`SELECT count(*) FROM shopping_list_items WHERE shopping_list_id = $1`, list.ID))
}

// Reclassifying is not idempotent: the job is discarded by it, so a second
// attempt has nothing to read and writes nothing.
func TestReclassifyingTheSameJobTwiceWritesOneList(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	job := doneJobWithPhoto(t, ctx, s, storageID)

	_, _, _, err := s.CreateShoppingListFromJob(ctx, storageID, job.ID, nil, lines("milk"))
	require.NoError(t, err)

	_, _, _, err = s.CreateShoppingListFromJob(ctx, storageID, job.ID, nil, lines("milk"))
	require.ErrorIs(t, err, store.ErrNotFound)

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM shopping_lists WHERE storage_id = $1`, storageID))
}

// A job in another storage is ErrNotFound, exactly like one that does not
// exist — the invariant PostgreSQL cannot express
// (docs/specs/03-auth-and-multi-tenancy.md).
func TestReclassifyingRefusesAJobFromAnotherStorage(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)
	foreign := doneJobWithPhoto(t, ctx, s, storageB)

	_, _, _, err := s.CreateShoppingListFromJob(ctx, storageA, foreign.ID, nil, lines("milk"))
	require.ErrorIs(t, err, store.ErrNotFound)

	_, _, _, missing := s.CreateShoppingListFromJob(ctx, storageA, uuid.New(), nil, lines("milk"))
	require.ErrorIs(t, missing, store.ErrNotFound, "and indistinguishable from an id that names nothing")

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM shopping_lists WHERE storage_id = $1`, storageA))
	_, err = s.Job(ctx, storageB, foreign.ID)
	assert.NoError(t, err, "the other storage's job is untouched")
}

// Only a proposal still waiting for review may be reclassified: one already
// applied, and one still being analysed, each have nothing to transfer.
func TestReclassifyingNeedsAJobWaitingForReview(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	pending, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	_, _, _, err = s.CreateShoppingListFromJob(ctx, storageID, pending.ID, nil, lines("milk"))
	assert.ErrorIs(t, err, store.ErrNotFound, "still being analysed")

	failed, err := s.CreateJob(ctx, store.NewJob{StorageID: storageID, Kind: store.JobShelfIngestion})
	require.NoError(t, err)
	require.NoError(t, s.FailJob(ctx, failed.ID, "unreadable"))
	_, _, _, err = s.CreateShoppingListFromJob(ctx, storageID, failed.ID, nil, lines("milk"))
	assert.ErrorIs(t, err, store.ErrNotFound, "analysis failed")

	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM shopping_lists WHERE storage_id = $1`, storageID))
	assert.Equal(t, 2, countRows(t, ctx,
		`SELECT count(*) FROM jobs WHERE storage_id = $1`, storageID), "a refused reclassification discards nothing")
}

// All or nothing: a line naming a product in another storage fails the whole
// call, and the job it would have discarded is still there.
func TestAFailedReclassificationDiscardsNothing(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, storageB := twoStorages(t, ctx)
	job := doneJobWithPhoto(t, ctx, s, storageA)

	foreign, err := s.CreateProduct(ctx, storageB, store.NewProduct{Name: "Their Milk"})
	require.NoError(t, err)

	bad := lines("milk")
	bad[0].MatchedProductID = &foreign.ID
	_, _, _, err = s.CreateShoppingListFromJob(ctx, storageA, job.ID, nil, bad)
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.Job(ctx, storageA, job.ID)
	assert.NoError(t, err, "the job survives a reclassification that did not happen")
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM shopping_lists WHERE storage_id = $1`, storageA))
}

func TestReclassifyingNeedsAtLeastOneLine(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	job := doneJobWithPhoto(t, ctx, s, storageID)

	_, _, _, err := s.CreateShoppingListFromJob(ctx, storageID, job.ID, nil, nil)
	require.ErrorIs(t, err, store.ErrValidation)

	_, err = s.Job(ctx, storageID, job.ID)
	assert.NoError(t, err)
}
