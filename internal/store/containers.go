package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Container is what a batch is physically held in — a 24-pack box, a bag, a
// cooler someone packed for a trip (docs/specs/39-batch-containers.md).
//
// It is deliberately not a location and not a second hierarchy: a container
// holds exactly the one batch that references it, and its remaining count *is*
// that batch's quantity. There is no count field here, because a second number
// is a number that can drift from the truth.
type Container struct {
	ID            uuid.UUID
	StorageID     uuid.UUID
	Label         string
	ContainerType *string
	CreatedAt     time.Time
	DestroyedAt   *time.Time
}

// ContainerDisposition says what happens to a container when the batch holding
// it is split (docs/specs/39-batch-containers.md, "Splitting a
// container-holding batch").
//
// The zero value is not a valid disposition; SplitBatch resolves an empty
// string to DispositionSource, which is the default the spec names and the
// common case it describes — two beers into the fridge, and the 24-pack that
// now holds 22 is still the 24-pack.
type ContainerDisposition string

// ContainerDisposition values. The set is closed: anything else is
// ErrValidation, and the handler refuses it before a transaction opens.
const (
	// DispositionSource leaves the container on the batch that stays behind;
	// the split-off portion gets none. The default.
	DispositionSource ContainerDisposition = "source"
	// DispositionTarget hands the container to the split-off portion and
	// leaves the source batch without one.
	DispositionTarget ContainerDisposition = "target"
	// DispositionBoth points both batches at the same container row.
	DispositionBoth ContainerDisposition = "both"
	// DispositionNeither leaves both batches without a container, without
	// destroying the container row.
	DispositionNeither ContainerDisposition = "neither"
	// DispositionDestroy destroys the container in the same transaction as the
	// split, clearing it off both batches.
	DispositionDestroy ContainerDisposition = "destroy"
)

// ValidContainerDisposition reports whether d is one of the five the spec
// names. Exported so the handler can refuse an unknown value with a 422 before
// opening a transaction, rather than discovering it inside one.
func ValidContainerDisposition(d ContainerDisposition) bool {
	switch d {
	case DispositionSource, DispositionTarget, DispositionBoth, DispositionNeither, DispositionDestroy:
		return true
	default:
		return false
	}
}

// DestroyContainer marks a container destroyed and clears it off every batch
// that referenced it, in one transaction
// (docs/specs/39-batch-containers.md, "Destroying a container").
//
// Destroying is **not** idempotent, and the refusals are deliberately
// indistinguishable from one another: an unknown id, an id belonging to another
// storage, and an id in this storage that is already destroyed all answer
// ErrNotFound — which the API turns into the same 404, never a 403 and never a
// 409. This endpoint's id arrives straight from the URL, so unlike the
// container_label rename path (which can only ever reach the batch's *own*
// container) it is the one container operation that needs a real runtime
// same-storage check.
//
// No inventory_logs row is written, here or anywhere else containers are
// touched: nothing about a quantity changed, only which physical object a batch
// is held in. containers.destroyed_at is itself the audit trail for "when was
// this container destroyed".
func (s *Store) DestroyContainer(ctx context.Context, storageID, containerID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return destroyContainer(ctx, tx, storageID, containerID)
	})
}

// destroyContainer is DestroyContainer inside a caller's transaction, so that a
// split asking for DispositionDestroy destroys the container in the same
// transaction as the split itself — never a split followed by a second
// statement a client could see between, or fail to make.
func destroyContainer(ctx context.Context, tx pgx.Tx, storageID, containerID uuid.UUID) error {
	// One statement covers all three refusals. Splitting it into a lookup and
	// an update would invite a branch per case, and a branch per case is how
	// "already destroyed" ends up answering something other than 404.
	tag, err := tx.Exec(ctx, `
		UPDATE containers
		   SET destroyed_at = now()
		 WHERE id = $1 AND storage_id = $2 AND destroyed_at IS NULL`,
		containerID, storageID)
	if err != nil {
		return fmt.Errorf("store: destroy container: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	// Scoped by container_id alone: a container id is unique, and the row it
	// names has just been confirmed to belong to storageID, so every batch
	// pointing at it is in this storage by construction.
	if _, err := tx.Exec(ctx,
		`UPDATE inventory_batches SET container_id = NULL WHERE container_id = $1`,
		containerID); err != nil {
		return fmt.Errorf("store: clear destroyed container from batches: %w", err)
	}
	return nil
}

// upsertBatchContainerLabel gives a batch a container with this label, creating
// the container on the first call and **renaming the same row** on every later
// one (docs/specs/39-batch-containers.md, "Setting and clearing a container on
// a batch").
//
// Renaming rather than creating a second container is the whole point: a label
// is metadata about one physical object, so fixing a typo must not leave two
// rows behind, one of them referenced by nothing.
//
// It needs no same-storage check of its own. There is no way to pass a
// container id through this path — only a label, upserted against whichever
// container the batch already points at — and the batch itself was resolved
// against the URL's storage by the caller before this runs.
func upsertBatchContainerLabel(ctx context.Context, tx pgx.Tx, storageID, batchID uuid.UUID, label string) error {
	current, err := batchContainerID(ctx, tx, batchID)
	if err != nil {
		return err
	}

	if current != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE containers SET label = $1 WHERE id = $2 AND storage_id = $3`,
			label, *current, storageID); err != nil {
			return fmt.Errorf("store: rename container: %w", err)
		}
		return nil
	}

	id, err := newID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO containers (id, storage_id, label) VALUES ($1, $2, $3)`,
		id, storageID, label); err != nil {
		return fmt.Errorf("store: create container: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE inventory_batches SET container_id = $1 WHERE id = $2`, id, batchID); err != nil {
		return fmt.Errorf("store: attach container to batch: %w", err)
	}
	return nil
}

// detachBatchContainer clears a batch's container_id without destroying the
// container row.
//
// This is "I labelled the wrong batch", not "the box is gone" — the latter is
// destroyContainer. The row survives, referenced by nothing, in case another
// batch should have pointed at it instead.
func detachBatchContainer(ctx context.Context, tx pgx.Tx, batchID uuid.UUID) error {
	if _, err := tx.Exec(ctx,
		`UPDATE inventory_batches SET container_id = NULL WHERE id = $1`, batchID); err != nil {
		return fmt.Errorf("store: detach container from batch: %w", err)
	}
	return nil
}

// setBatchContainerType writes the descriptive type onto whichever container
// the batch points at now — after any label upsert in the same patch, so
// {container_label, container_type} arriving together works on a batch that had
// no container before.
//
// A batch with no container is ErrValidation, which the API answers 422: there
// is nothing to attach the type to. That is the spec's rule, and it holds for a
// null type as much as for a string — "clear the type of the container this
// batch does not have" is the same empty statement either way.
func setBatchContainerType(ctx context.Context, tx pgx.Tx, storageID, batchID uuid.UUID, containerType *string) error {
	current, err := batchContainerID(ctx, tx, batchID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("%w: container_type needs a container — send container_label too", ErrValidation)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE containers SET container_type = $1 WHERE id = $2 AND storage_id = $3`,
		containerType, *current, storageID); err != nil {
		return fmt.Errorf("store: set container type: %w", err)
	}
	return nil
}

// batchContainerID reads the container a batch currently points at, if any.
//
// FOR UPDATE for the same reason every other write path in batches.go locks
// the row it is about to change: this is a read the very next statement acts
// on. Without it two concurrent "name this container" patches on the same
// container-less batch both see NULL, both insert, and one of the two
// containers is immediately an orphan nothing references — harmless to the
// stock, but a row nobody asked for and nobody can find again.
//
// Unscoped by storage on purpose: every caller has already resolved the batch
// against the URL's storage, and repeating the join here would make the
// container paths look like they carry a check they do not — the structural
// argument in docs/specs/39-batch-containers.md's acceptance criteria is the
// one that holds for them.
func batchContainerID(ctx context.Context, tx pgx.Tx, batchID uuid.UUID) (*uuid.UUID, error) {
	var containerID *uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT container_id FROM inventory_batches WHERE id = $1 FOR UPDATE`, batchID).Scan(&containerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: load batch container: %w", err)
	}
	return containerID, nil
}
