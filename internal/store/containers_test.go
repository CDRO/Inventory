package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// docs/specs/39-batch-containers.md, against a real database.
//
// Two rules here fail silently in the way CLAUDE.md warns about. The first is
// that renaming a container must rename the row rather than create a second one:
// a label that creates a new container each time looks perfectly correct from
// the batch it was set on, and only shows up as a pile of orphans nothing
// references. The second is that **no container operation writes an
// inventory_logs row** — nothing about a quantity changed — which a test has to
// assert as an absence, because the ledger is never consulted by the code under
// test.

// container reads a container row straight from the database, so the assertions
// below are about what was stored rather than about what a store method chose to
// return.
type container struct {
	ID            uuid.UUID
	Label         string
	ContainerType *string
	DestroyedAt   *time.Time
}

func readContainer(t *testing.T, ctx context.Context, id uuid.UUID) container {
	t.Helper()

	var c container
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT id, label, container_type, destroyed_at FROM containers WHERE id = $1`, id).
		Scan(&c.ID, &c.Label, &c.ContainerType, &c.DestroyedAt))
	return c
}

func containerCount(t *testing.T, ctx context.Context, storageID uuid.UUID) int {
	t.Helper()
	return countRows(t, ctx, `SELECT count(*) FROM containers WHERE storage_id = $1`, storageID)
}

// label is BatchPatch's container_label as a caller sends it: present, non-null.
func label(s string) store.BatchPatch {
	return store.BatchPatch{SetContainerLabel: true, ContainerLabel: &s}
}

// detach is container_label: null — take the batch out of its container without
// destroying the container.
func detach() store.BatchPatch {
	return store.BatchPatch{SetContainerLabel: true}
}

func TestSettingAContainerLabelCreatesAndAttachesOne(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 24)

	before := logCount(t, ctx, productID)

	updated, err := s.UpdateBatch(ctx, storageID, batchID, label("24-pack box"), nil)
	require.NoError(t, err)

	require.NotNil(t, updated.ContainerID)
	require.NotNil(t, updated.ContainerLabel)
	assert.Equal(t, "24-pack box", *updated.ContainerLabel,
		"the label has to come back joined, not as an id the caller must resolve")
	assert.Nil(t, updated.ContainerType, "no type was sent")
	assert.Equal(t, 24, updated.Quantity, "a container says nothing about a quantity")

	stored := readContainer(t, ctx, *updated.ContainerID)
	assert.Equal(t, "24-pack box", stored.Label)
	assert.Nil(t, stored.DestroyedAt)

	assert.Equal(t, before, logCount(t, ctx, productID),
		"no inventory_logs row: nothing about a quantity changed")
}

// TestRenamingAContainerKeepsTheSameRow is the acceptance criterion a
// create-on-every-set implementation passes every other test with.
func TestRenamingAContainerKeepsTheSameRow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 24)
	before := logCount(t, ctx, productID)

	first, err := s.UpdateBatch(ctx, storageID, batchID, label("24-pak box"), nil)
	require.NoError(t, err)
	require.NotNil(t, first.ContainerID)

	renamed, err := s.UpdateBatch(ctx, storageID, batchID, label("24-pack box"), nil)
	require.NoError(t, err)
	require.NotNil(t, renamed.ContainerID)

	assert.Equal(t, *first.ContainerID, *renamed.ContainerID,
		"fixing a typo must rename the physical object, not invent a second one")
	assert.Equal(t, "24-pack box", readContainer(t, ctx, *first.ContainerID).Label)
	assert.Equal(t, 1, containerCount(t, ctx, storageID),
		"a rename that created a second row would leave an orphan behind")
	assert.Equal(t, before, logCount(t, ctx, productID),
		"neither the create nor the rename writes an inventory_logs row")
}

// TestClearingAContainerLabelDetachesWithoutDestroying separates the two things
// a person can mean: "I labelled the wrong batch" leaves the box in the world,
// "I threw the box away" is the destroy endpoint.
func TestClearingAContainerLabelDetachesWithoutDestroying(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 24)

	attached, err := s.UpdateBatch(ctx, storageID, batchID, label("24-pack box"), nil)
	require.NoError(t, err)
	containerID := *attached.ContainerID
	before := logCount(t, ctx, productID)

	cleared, err := s.UpdateBatch(ctx, storageID, batchID, detach(), nil)
	require.NoError(t, err)

	assert.Nil(t, cleared.ContainerID)
	assert.Nil(t, cleared.ContainerLabel)

	survivor := readContainer(t, ctx, containerID)
	assert.Equal(t, "24-pack box", survivor.Label, "the container row survives a detach")
	assert.Nil(t, survivor.DestroyedAt, "a detach is not a destroy")
	assert.Equal(t, before, logCount(t, ctx, productID))
}

func TestContainerTypeNeedsAContainer(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, batchID := stocked(t, ctx, s, 24)

	kind := "box"

	t.Run("alone on a batch with no container", func(t *testing.T) {
		_, err := s.UpdateBatch(ctx, storageID, batchID,
			store.BatchPatch{SetContainerType: true, ContainerType: &kind}, nil)
		require.ErrorIs(t, err, store.ErrValidation,
			"there is nothing to attach the type to")
		assert.Equal(t, 0, containerCount(t, ctx, storageID),
			"the refused patch must not have created one on the way")
	})

	t.Run("together with a label, on the container that patch creates", func(t *testing.T) {
		updated, err := s.UpdateBatch(ctx, storageID, batchID, store.BatchPatch{
			SetContainerLabel: true, ContainerLabel: ptr("24-pack box"),
			SetContainerType: true, ContainerType: &kind,
		}, nil)
		require.NoError(t, err)
		require.NotNil(t, updated.ContainerType)
		assert.Equal(t, "box", *updated.ContainerType)
	})

	t.Run("cleared on an existing container", func(t *testing.T) {
		updated, err := s.UpdateBatch(ctx, storageID, batchID,
			store.BatchPatch{SetContainerType: true}, nil)
		require.NoError(t, err)
		assert.Nil(t, updated.ContainerType)
		require.NotNil(t, updated.ContainerLabel, "clearing the type is not a detach")
	})
}

// TestContainerPatchOnAForeignBatchIs404 is the structural half of spec 39's
// same-storage argument: container_label never takes a container id, so the only
// id it can be attacked through is the batch's own, which is checked before any
// container row is written.
func TestContainerPatchOnAForeignBatchIs404(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA := newStorage(t, ctx)
	_, _, _, foreignBatch := stocked(t, ctx, s, 24)

	_, err := s.UpdateBatch(ctx, storageA, foreignBatch, label("Their box"), nil)

	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 0, containerCount(t, ctx, storageA),
		"a refused patch must not leave a container behind in the caller's storage")
}

// TestEmptyingABatchCannotAlsoChangeItsContainer: the row is deleted by the
// quantity half, so the container half would have nothing to act on. Refusing is
// the only reading that cannot silently pick one — the same rule the
// zero-quantity-plus-move combination already has.
func TestEmptyingABatchCannotAlsoChangeItsContainer(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, batchID := stocked(t, ctx, s, 3)

	zero := 0
	patch := label("24-pack box")
	patch.Quantity = &zero

	_, err := s.UpdateBatch(ctx, storageID, batchID, patch, nil)

	require.ErrorIs(t, err, store.ErrValidation)
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE id = $1`, batchID),
		"the refused patch must not have deleted the batch either")
}

func TestDestroyContainerClearsEveryReferencingBatch(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 24)
	fridge, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
	require.NoError(t, err)

	attached, err := s.UpdateBatch(ctx, storageID, batchID, label("24-pack box"), nil)
	require.NoError(t, err)
	containerID := *attached.ContainerID

	// "both" is the only way two batches end up sharing one container, which is
	// what makes the sweep more than a single-row update.
	shared, err := s.SplitBatch(ctx, storageID, batchID, store.SplitBatchInput{
		Quantity: 2, TargetLocationID: fridge.ID, ContainerDisposition: store.DispositionBoth,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, shared.ContainerID)
	require.Equal(t, containerID, *shared.ContainerID)

	before := logCount(t, ctx, productID)

	require.NoError(t, s.DestroyContainer(ctx, storageID, containerID))

	destroyed := readContainer(t, ctx, containerID)
	require.NotNil(t, destroyed.DestroyedAt, "destroy stamps destroyed_at")
	assert.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE container_id = $1`, containerID),
		"every batch that referenced it is cleared, not just the one in view")
	assert.Equal(t, 2, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE product_id = $1`, productID),
		"destroying a container deletes no stock")
	assert.Equal(t, before, logCount(t, ctx, productID),
		"destroyed_at is the audit trail; the ledger explains quantities")
}

// TestDestroyContainerRefusalsAreIndistinguishable is the security property, not
// just the refusal: an unknown id, an id from another storage and an
// already-destroyed id must all be the same error, or a caller can probe ids and
// learn which name real containers in storages they cannot see
// (docs/specs/03-auth-and-multi-tenancy.md).
func TestDestroyContainerRefusalsAreIndistinguishable(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageA, _, _, batchA := stocked(t, ctx, s, 5)
	storageB, _, _, batchB := stocked(t, ctx, s, 5)

	mine, err := s.UpdateBatch(ctx, storageA, batchA, label("Mine"), nil)
	require.NoError(t, err)
	theirs, err := s.UpdateBatch(ctx, storageB, batchB, label("Theirs"), nil)
	require.NoError(t, err)

	require.NoError(t, s.DestroyContainer(ctx, storageA, *mine.ContainerID))

	unknown := s.DestroyContainer(ctx, storageA, uuid.New())
	foreign := s.DestroyContainer(ctx, storageA, *theirs.ContainerID)
	again := s.DestroyContainer(ctx, storageA, *mine.ContainerID)

	require.ErrorIs(t, unknown, store.ErrNotFound)
	require.ErrorIs(t, foreign, store.ErrNotFound)
	require.ErrorIs(t, again, store.ErrNotFound,
		"destroying is not idempotent: a second call is 'already gone'")
	assert.Equal(t, unknown.Error(), foreign.Error(),
		"a foreign id must not be distinguishable from one that never existed")
	assert.Equal(t, unknown.Error(), again.Error())

	// And the refusal is a refusal: the other storage's container is untouched,
	// and so is the batch that holds it.
	assert.Nil(t, readContainer(t, ctx, *theirs.ContainerID).DestroyedAt)
	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE container_id = $1`, *theirs.ContainerID))
}

// TestConsumingAContainerToNothingLeavesItStanding: emptying a container by
// consuming everything in it is not the same statement as throwing the box away,
// so the row stays, undestroyed, referenced by nothing
// (docs/specs/39-batch-containers.md, "Schema").
func TestConsumingAContainerToNothingLeavesItStanding(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, batchID := stocked(t, ctx, s, 2)

	attached, err := s.UpdateBatch(ctx, storageID, batchID, label("Rice bag"), nil)
	require.NoError(t, err)
	containerID := *attached.ContainerID

	require.NoError(t, s.AdjustBatch(ctx, storageID, batchID, -2, store.ReasonConsumption, nil))
	require.Equal(t, 0, countRows(t, ctx,
		`SELECT count(*) FROM inventory_batches WHERE id = $1`, batchID),
		"a batch at zero is deleted")

	orphan := readContainer(t, ctx, containerID)
	assert.Equal(t, "Rice bag", orphan.Label)
	assert.Nil(t, orphan.DestroyedAt,
		"nothing sweeps or auto-destroys a container whose stock ran out")
}

// TestSplitContainerDispositions walks the exact table in
// docs/specs/39-batch-containers.md's Splitting section, one subtest per row.
//
// The scenario is the one the spec exists for: 24 in the pack, two carried to
// the fridge, and the question of where the pack goes.
func TestSplitContainerDispositions(t *testing.T) {
	tests := []struct {
		disposition   store.ContainerDisposition
		sourceKeepsIt bool
		targetGetsIt  bool
		destroyed     bool
	}{
		{store.DispositionSource, true, false, false},
		{store.DispositionTarget, false, true, false},
		{store.DispositionBoth, true, true, false},
		{store.DispositionNeither, false, false, false},
		{store.DispositionDestroy, false, false, true},
		// The empty disposition is what a client that omits the field sends,
		// and it has to resolve to "source" rather than to "no container
		// anywhere".
		{"", true, false, false},
	}

	for _, tc := range tests {
		name := string(tc.disposition)
		if name == "" {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			s := requireDB(t)
			ctx := context.Background()
			storageID, productID, _, batchID := stocked(t, ctx, s, 24)
			fridge, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
			require.NoError(t, err)

			attached, err := s.UpdateBatch(ctx, storageID, batchID, label("24-pack box"), nil)
			require.NoError(t, err)
			containerID := *attached.ContainerID
			logsBefore := logCount(t, ctx, productID)

			created, err := s.SplitBatch(ctx, storageID, batchID, store.SplitBatchInput{
				Quantity: 2, TargetLocationID: fridge.ID, ContainerDisposition: tc.disposition,
			}, nil)
			require.NoError(t, err)

			source, err := s.ListProductBatches(ctx, storageID, productID)
			require.NoError(t, err)
			var sourceContainer *uuid.UUID
			for _, b := range source {
				if b.ID == batchID {
					sourceContainer = b.ContainerID
					assert.Equal(t, 22, b.Quantity, "the pack now holds 22")
				}
			}

			if tc.sourceKeepsIt {
				require.NotNil(t, sourceContainer)
				assert.Equal(t, containerID, *sourceContainer)
			} else {
				assert.Nil(t, sourceContainer)
			}

			if tc.targetGetsIt {
				require.NotNil(t, created.ContainerID)
				assert.Equal(t, containerID, *created.ContainerID)
				require.NotNil(t, created.ContainerLabel)
				assert.Equal(t, "24-pack box", *created.ContainerLabel)
			} else {
				assert.Nil(t, created.ContainerID,
					"the two beers in the fridge are not a container unless somebody says so")
				assert.Nil(t, created.ContainerLabel)
			}

			assert.Equal(t, 2, created.Quantity)
			assert.Equal(t, tc.destroyed, readContainer(t, ctx, containerID).DestroyedAt != nil)

			// The split's own two 'move' rows and nothing else: the container
			// half of this operation writes no ledger row whatever it did.
			assert.Equal(t, logsBefore+2, logCount(t, ctx, productID),
				"a split writes exactly two move rows; the disposition writes none")
		})
	}
}

// TestSplitDispositionIsANoOpWithoutAContainer covers the explicit
// never-an-error rule: a frontend that always sends the field for symmetry must
// not have to special-case container-less batches, and "destroy" has nothing to
// destroy.
func TestSplitDispositionIsANoOpWithoutAContainer(t *testing.T) {
	for _, disposition := range []store.ContainerDisposition{
		store.DispositionSource, store.DispositionTarget, store.DispositionBoth,
		store.DispositionNeither, store.DispositionDestroy,
	} {
		t.Run(string(disposition), func(t *testing.T) {
			s := requireDB(t)
			ctx := context.Background()
			storageID, productID, _, batchID := stocked(t, ctx, s, 5)
			fridge, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
			require.NoError(t, err)

			created, err := s.SplitBatch(ctx, storageID, batchID, store.SplitBatchInput{
				Quantity: 1, TargetLocationID: fridge.ID, ContainerDisposition: disposition,
			}, nil)

			require.NoError(t, err, "a container-less source makes every disposition a no-op")
			assert.Nil(t, created.ContainerID)
			assert.Equal(t, 0, containerCount(t, ctx, storageID),
				"a no-op must not invent a container to dispose of")
			batches, err := s.ListProductBatches(ctx, storageID, productID)
			require.NoError(t, err)
			assert.Len(t, batches, 2)
		})
	}
}

func TestSplitRejectsAnUnknownContainerDisposition(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 5)
	fridge, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
	require.NoError(t, err)

	_, err = s.SplitBatch(ctx, storageID, batchID, store.SplitBatchInput{
		Quantity: 1, TargetLocationID: fridge.ID, ContainerDisposition: "keep",
	}, nil)

	require.ErrorIs(t, err, store.ErrValidation)
	batches, err := s.ListProductBatches(ctx, storageID, productID)
	require.NoError(t, err)
	assert.Len(t, batches, 1, "the refused split must not have happened")
}

// TestARefusedSplitChangesNothingAtAll pins the ordering, not the atomicity: a
// split refused for any reason — here a target location in another storage —
// leaves the source's quantity, its location and its container exactly as they
// were, because the disposition never runs on a split that does not happen.
//
// It deliberately does not claim to observe the transaction. "destroy" sharing
// the split's transaction is a property of both writes running inside the same
// s.inTx, and no test outside that transaction can see a moment where one
// landed and the other had not — which is the point of it being one
// transaction. What this test would catch is the disposition being moved ahead
// of the validation that refuses the split.
func TestARefusedSplitChangesNothingAtAll(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, batchID := stocked(t, ctx, s, 24)
	foreignStorage := newStorage(t, ctx)
	foreignShelf, err := s.CreateLocation(ctx, foreignStorage, store.NewLocation{Name: "Their Fridge"})
	require.NoError(t, err)

	attached, err := s.UpdateBatch(ctx, storageID, batchID, label("24-pack box"), nil)
	require.NoError(t, err)
	containerID := *attached.ContainerID

	_, err = s.SplitBatch(ctx, storageID, batchID, store.SplitBatchInput{
		Quantity: 2, TargetLocationID: foreignShelf.ID, ContainerDisposition: store.DispositionDestroy,
	}, nil)
	require.ErrorIs(t, err, store.ErrNotFound)

	survivor := readContainer(t, ctx, containerID)
	assert.Nil(t, survivor.DestroyedAt,
		"the disposition must not run on a split that was refused before it began")
	batches, err := s.ListProductBatches(ctx, storageID, productID)
	require.NoError(t, err)
	require.Len(t, batches, 1)
	assert.Equal(t, 24, batches[0].Quantity, "and moves nothing")
	require.NotNil(t, batches[0].ContainerID)
	assert.Equal(t, containerID, *batches[0].ContainerID)
}

// TestMovingAWholeBatchKeepsItsContainer: the object physically travels with the
// batch, so a move touches no container field (docs/specs/39-batch-containers.md).
func TestMovingAWholeBatchKeepsItsContainer(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, _, _, batchID := stocked(t, ctx, s, 24)
	cellar, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Cellar"})
	require.NoError(t, err)

	attached, err := s.UpdateBatch(ctx, storageID, batchID, label("24-pack box"), nil)
	require.NoError(t, err)

	moved, err := s.UpdateBatch(ctx, storageID, batchID,
		store.BatchPatch{LocationID: &cellar.ID}, nil)
	require.NoError(t, err)

	assert.Equal(t, cellar.ID, moved.LocationID)
	require.NotNil(t, moved.ContainerID)
	assert.Equal(t, *attached.ContainerID, *moved.ContainerID)
	require.NotNil(t, moved.ContainerLabel)
	assert.Equal(t, "24-pack box", *moved.ContainerLabel)
}

// TestTheSpecsOwnScenario is the shape docs/specs/39-batch-containers.md exists
// for, end to end: 24 in the pack, two carried to the fridge, 22 left in the
// pack, and the two in the fridge in nothing at all.
func TestTheSpecsOwnScenario(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID, productID, _, packID := stocked(t, ctx, s, 24)
	fridge, err := s.CreateLocation(ctx, storageID, store.NewLocation{Name: "Fridge"})
	require.NoError(t, err)

	_, err = s.UpdateBatch(ctx, storageID, packID, store.BatchPatch{
		SetContainerLabel: true, ContainerLabel: ptr("24-pack box"),
		SetContainerType: true, ContainerType: ptr("box"),
	}, nil)
	require.NoError(t, err)

	_, err = s.SplitBatch(ctx, storageID, packID,
		store.SplitBatchInput{Quantity: 2, TargetLocationID: fridge.ID}, nil)
	require.NoError(t, err)

	batches, err := s.ListProductBatches(ctx, storageID, productID)
	require.NoError(t, err)
	require.Len(t, batches, 2)

	var total int
	for _, b := range batches {
		total += b.Quantity
		switch b.ID {
		case packID:
			assert.Equal(t, 22, b.Quantity)
			require.NotNil(t, b.ContainerLabel)
			assert.Equal(t, "24-pack box", *b.ContainerLabel)
			require.NotNil(t, b.ContainerType)
			assert.Equal(t, "box", *b.ContainerType)
		default:
			assert.Equal(t, 2, b.Quantity)
			assert.Nil(t, b.ContainerID, "two beers in the fridge are not a container")
		}
	}
	assert.Equal(t, 24, total, "a split moves stock, it does not create or destroy it")
}

func ptr[T any](v T) *T { return &v }
