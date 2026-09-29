package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/expiry"
)

// LogReason is why a quantity changed. The set matches the CHECK on
// inventory_logs.reason.
type LogReason string

// LogReason values.
const (
	ReasonPurchase        LogReason = "purchase"
	ReasonConsumption     LogReason = "consumption"
	ReasonAudit           LogReason = "audit"
	ReasonVisionIngestion LogReason = "vision_ingestion"
	ReasonMove            LogReason = "move"
)

// ExpirationSource records whether a date was computed from the shelf-life
// rules or typed by a person. The distinction is what makes the cascade in
// docs/specs/08-expiration-and-classification.md safe.
type ExpirationSource string

// ExpirationSource values.
const (
	ExpirationDerived ExpirationSource = "derived"
	ExpirationUser    ExpirationSource = "user"
)

// Batch is a quantity of one product at exactly one location.
//
// ContainerID is what the batch is physically held in, if anything
// (docs/specs/39-batch-containers.md) — orthogonal to LocationID, which is
// where that holding happens. ContainerLabel and ContainerType are that
// container's own columns, joined in by the read paths that have a container to
// join (loadBatch, ListProductBatches) and left nil by the ones that cannot:
// an INSERT ... RETURNING cannot join, so a batch straight out of createBatch
// carries its container id and no label. Every caller that hands a Batch to a
// client goes through a joining path.
type Batch struct {
	ID               uuid.UUID
	ProductID        uuid.UUID
	LocationID       uuid.UUID
	Quantity         int
	ExpirationDate   *time.Time
	ExpirationSource ExpirationSource
	CreatedAt        time.Time
	ContainerID      *uuid.UUID
	ContainerLabel   *string
	ContainerType    *string
}

// NewBatch is the input to CreateBatch.
//
// ContainerLabel and ContainerType are the same upsert-on-creation fields
// docs/specs/39-batch-containers.md's closing paragraph describes for
// creation-time containers: the same field names and the same upsert rule
// the PATCH already has, just applied to a batch that does not exist yet
// instead of one already on the shelf. A nil ContainerLabel with a non-nil
// ContainerType is the same 422 the PATCH gives — there is nothing to attach
// the type to.
type NewBatch struct {
	ProductID        uuid.UUID
	LocationID       uuid.UUID
	Quantity         int
	ExpirationDate   *time.Time
	ExpirationSource ExpirationSource
	Reason           LogReason
	CreatedBy        *uuid.UUID
	ContainerLabel   *string
	ContainerType    *string
}

// CreateBatch inserts a batch and its paired inventory_logs row in one
// transaction.
//
// Both the product and the location are checked against storageID. The
// location check is the invariant PostgreSQL cannot express: locations and
// products both carry storage_id, but inventory_batches.location_id is a plain
// foreign key, so without this a batch could be filed against a shelf in
// somebody else's house.
func (s *Store) CreateBatch(ctx context.Context, storageID uuid.UUID, in NewBatch) (*Batch, error) {
	var out *Batch
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		batch, err := createBatch(ctx, tx, storageID, in)
		out = batch
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// createBatch is CreateBatch inside a caller's transaction, for writes that
// must land together with others — a confirmed ingestion proposal, say.
func createBatch(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, in NewBatch) (*Batch, error) {
	// A batch is a quantity of something in a place, so it starts at one or
	// more. Allowing zero would create a row the model says should not exist —
	// AdjustBatch deletes a batch the moment it reaches zero — and would pair
	// it with a ledger entry recording that nothing happened. A product with
	// no stock is a product with no batches, which is what the reorder
	// dashboard in spec 10 reads.
	if in.Quantity < 1 {
		return nil, fmt.Errorf("%w: batch quantity must be at least 1, got %d", ErrValidation, in.Quantity)
	}
	if in.Reason == "" {
		return nil, fmt.Errorf("%w: a batch write needs a log reason", ErrValidation)
	}
	if in.ExpirationSource == "" {
		in.ExpirationSource = ExpirationDerived
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}

	if err := requireProductInStorage(ctx, tx, storageID, in.ProductID); err != nil {
		return nil, err
	}
	if err := requireSameStorage(ctx, tx, treeLocations, storageID, in.LocationID); err != nil {
		return nil, err
	}

	// A batch created without an explicit date gets the resolved default
	// rather than no date at all
	// (docs/specs/08-expiration-and-classification.md): "never leaves it
	// unset by omission".
	//
	// The guard is on the *source*, not on the date being nil, because nil
	// means two different things. A caller that says ExpirationUser is
	// making a deliberate statement — including "this has no expiry" — and
	// resolving over the top of that would be the exact behaviour the
	// derived/user distinction exists to prevent. Anything else is an
	// omission, and an omission is what the rules are for.
	if in.ExpirationDate == nil && in.ExpirationSource != ExpirationUser {
		rules, err := expiryRulesFor(ctx, tx, storageID, in.ProductID)
		if err != nil {
			return nil, err
		}
		// created_at defaults to now() in the same statement below, so the
		// date is computed from the same clock the row will carry.
		in.ExpirationDate = expiry.DateFor(time.Now(), expiry.Resolve(rules))
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_date, expiration_source)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at, container_id`,
		id, in.ProductID, in.LocationID, in.Quantity, in.ExpirationDate, string(in.ExpirationSource))

	batch, err := scanBatch(row)
	if err != nil {
		return nil, err
	}

	// Same upsert rule the PATCH has (docs/specs/39-batch-containers.md,
	// "Setting and clearing a container on a batch"), just applied to a batch
	// that has never had a container before: a fresh batch never already has
	// one, so upsertBatchContainerLabel always creates rather than renames, and
	// setBatchContainerType always follows it in the same call rather than
	// landing on an existing container. No inventory_logs row here — same as
	// every other container write, nothing about quantity changed.
	if in.ContainerLabel != nil {
		if err := upsertBatchContainerLabel(ctx, tx, storageID, batch.ID, *in.ContainerLabel); err != nil {
			return nil, err
		}
	}
	if in.ContainerType != nil {
		if err := setBatchContainerType(ctx, tx, storageID, batch.ID, in.ContainerType); err != nil {
			return nil, err
		}
	}

	if err := writeLog(ctx, tx, in.ProductID, &batch.ID, in.Quantity, in.Reason, in.CreatedBy); err != nil {
		return nil, err
	}
	if err := bumpForLedgerReason(ctx, tx, storageID, in.CreatedBy, in.Reason, batch.ID); err != nil {
		return nil, err
	}

	if in.ContainerLabel != nil || in.ContainerType != nil {
		// Re-read through the joining path so the created batch carries its
		// container's label, not just an id the caller would have to resolve —
		// the same reasoning SplitBatch's own re-read uses.
		return loadBatch(ctx, tx, storageID, batch.ID)
	}
	return batch, nil
}

// AdjustBatch changes a batch's quantity by delta and writes the paired log
// row in the same transaction.
//
// A batch that reaches exactly zero is deleted in that same transaction:
// batches represent physical things in a place, and one holding nothing is not
// a thing. Contrast products, which validly sit at zero total stock and feed
// the reorder dashboard.
//
// A delta that would take the batch below zero is rejected rather than
// clamped. Silently clamping would record a consumption that did not happen
// and leave the ledger disagreeing with the shelf.
func (s *Store) AdjustBatch(ctx context.Context, storageID, batchID uuid.UUID, delta int, reason LogReason, userID *uuid.UUID) error {
	if delta == 0 {
		return fmt.Errorf("%w: a zero adjustment writes a log row that explains nothing", ErrValidation)
	}
	if reason == "" {
		return fmt.Errorf("%w: a batch write needs a log reason", ErrValidation)
	}

	return s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := adjustBatch(ctx, tx, storageID, batchID, delta, reason, userID)
		return err
	})
}

// adjustBatch is AdjustBatch inside a caller's transaction, for writes that
// must land together with others — a confirmed consumption proposal, say
// (docs/specs/09-consumption-logging.md). It returns the batch's product id,
// so a caller that must also confirm the batch belongs to a particular
// product does not need a second query.
func adjustBatch(ctx context.Context, tx pgx.Tx, storageID, batchID uuid.UUID, delta int, reason LogReason, userID *uuid.UUID) (uuid.UUID, error) {
	var productID uuid.UUID
	var quantity int
	err := tx.QueryRow(ctx, `
		SELECT b.product_id, b.quantity
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE b.id = $1 AND p.storage_id = $2
		 FOR UPDATE OF b`, batchID, storageID).Scan(&productID, &quantity)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: load batch: %w", err)
	}

	updated := quantity + delta
	if updated < 0 {
		return uuid.Nil, fmt.Errorf("%w: batch holds %d, cannot apply %d", ErrValidation, quantity, delta)
	}

	if updated == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM inventory_batches WHERE id = $1`, batchID); err != nil {
			return uuid.Nil, fmt.Errorf("store: delete emptied batch: %w", err)
		}
		// batch_id is ON DELETE SET NULL, so the log keeps its meaning
		// after the row it pointed at is gone.
		if err := writeLog(ctx, tx, productID, nil, delta, reason, userID); err != nil {
			return uuid.Nil, err
		}
		if err := bumpForLedgerReason(ctx, tx, storageID, userID, reason, batchID); err != nil {
			return uuid.Nil, err
		}
		return productID, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE inventory_batches SET quantity = $1 WHERE id = $2`, updated, batchID); err != nil {
		return uuid.Nil, fmt.Errorf("store: update batch quantity: %w", err)
	}
	if err := writeLog(ctx, tx, productID, &batchID, delta, reason, userID); err != nil {
		return uuid.Nil, err
	}
	if err := bumpForLedgerReason(ctx, tx, storageID, userID, reason, batchID); err != nil {
		return uuid.Nil, err
	}
	return productID, nil
}

// ListProductBatches returns a product's batches, nearest expiration first —
// the default first-out order for the decrement picker in
// docs/specs/09-consumption-logging.md. A batch with no expiration date sorts
// last: it is not the one a reviewer should be steered towards using first.
func (s *Store) ListProductBatches(ctx context.Context, storageID, productID uuid.UUID) ([]Batch, error) {
	if err := requireProductInStorage(ctx, s.pool, storageID, productID); err != nil {
		return nil, err
	}

	// LEFT JOIN, not JOIN: a container is optional, and a batch without one
	// still belongs on this list. The join is what lets the product edit
	// surface show "24-pack box" instead of a UUID
	// (docs/specs/39-batch-containers.md, "Product detail UI").
	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.product_id, b.location_id, b.quantity, b.expiration_date,
		       b.expiration_source, b.created_at, b.container_id, c.label, c.container_type
		  FROM inventory_batches b
		  LEFT JOIN containers c ON c.id = b.container_id
		 WHERE b.product_id = $1
		 ORDER BY b.expiration_date NULLS LAST, b.created_at`, productID)
	if err != nil {
		return nil, fmt.Errorf("store: list product batches: %w", err)
	}
	defer rows.Close()

	out := []Batch{}
	for rows.Next() {
		b, err := scanBatchWithContainer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// SplitBatchInput is what a split needs beyond the batch it splits.
//
// A struct rather than three positional arguments: the disposition is the third
// thing to describe the same operation, and a signature that reads
// (quantity, target, disposition) is one a future fourth field turns into a
// puzzle at every call site.
type SplitBatchInput struct {
	// Quantity is how much moves to the target, strictly between 1 and the
	// source's current quantity − 1. Splitting the whole batch is a move.
	Quantity int
	// TargetLocationID is where the split-off portion lands. Checked against
	// the storage, like every other id a caller supplies.
	TargetLocationID uuid.UUID
	// ContainerDisposition decides what happens to the source's container, if
	// it has one. Empty means DispositionSource — the default the spec names.
	ContainerDisposition ContainerDisposition
}

// SplitBatch moves quantity units of a batch to another location.
//
// The physical case is three jars in the cellar and one carried to the
// kitchen. That is a split into two batches, never one batch with two homes.
//
// Both halves keep the original expiration_date *and* expiration_source: they
// are the same jars, so their expiry does not reset, and a date a person typed
// stays a date a person typed. Two log rows are written with reason 'move',
// summing to zero, so product totals are unchanged while per-location figures
// stay correct.
//
// The container the source is held in follows in.ContainerDisposition
// (docs/specs/39-batch-containers.md): by default it stays with the stock that
// stays behind — two beers into the fridge, and the 24-pack that now holds 22
// is still the 24-pack. A source batch with no container makes every
// disposition a no-op rather than an error, so a frontend that always sends the
// field for symmetry does not have to special-case container-less batches. No
// inventory_logs row is written for any of it: the container is not a quantity.
func (s *Store) SplitBatch(ctx context.Context, storageID, batchID uuid.UUID, in SplitBatchInput, userID *uuid.UUID) (*Batch, error) {
	disposition := in.ContainerDisposition
	if disposition == "" {
		disposition = DispositionSource
	}
	if !ValidContainerDisposition(disposition) {
		return nil, fmt.Errorf("%w: unknown container_disposition %q", ErrValidation, string(disposition))
	}

	newBatchID, err := newID()
	if err != nil {
		return nil, err
	}

	var out *Batch
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var source Batch
		var srcExpiration string
		err := tx.QueryRow(ctx, `
			SELECT b.id, b.product_id, b.location_id, b.quantity, b.expiration_date,
			       b.expiration_source, b.container_id
			  FROM inventory_batches b
			  JOIN products p ON p.id = b.product_id
			 WHERE b.id = $1 AND p.storage_id = $2
			 FOR UPDATE OF b`, batchID, storageID).
			Scan(&source.ID, &source.ProductID, &source.LocationID, &source.Quantity,
				&source.ExpirationDate, &srcExpiration, &source.ContainerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: load batch to split: %w", err)
		}
		source.ExpirationSource = ExpirationSource(srcExpiration)

		// Splitting the whole batch is a move, which is a different operation.
		if in.Quantity < 1 || in.Quantity > source.Quantity-1 {
			return fmt.Errorf("%w: split quantity must be between 1 and %d", ErrValidation, source.Quantity-1)
		}
		if err := requireSameStorage(ctx, tx, treeLocations, storageID, in.TargetLocationID); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE inventory_batches SET quantity = quantity - $1 WHERE id = $2`,
			in.Quantity, batchID); err != nil {
			return fmt.Errorf("store: debit source batch: %w", err)
		}

		// The source side of the disposition runs before the target row exists,
		// and the target is then inserted with whatever the table says it
		// should carry. That ordering is what keeps "destroy" honest: the
		// destroy sweep clears container_id on every batch referencing the
		// container, so a target inserted *before* it holding that container
		// would be cleared by accident rather than by the rule.
		targetContainerID := source.ContainerID
		if source.ContainerID != nil {
			switch disposition {
			case DispositionSource, DispositionBoth:
				// The source keeps it; nothing to write.
			case DispositionTarget, DispositionNeither:
				if err := detachBatchContainer(ctx, tx, batchID); err != nil {
					return err
				}
			case DispositionDestroy:
				// Clears the source too, since at this moment the source is
				// the only batch referencing it.
				if err := destroyContainer(ctx, tx, storageID, *source.ContainerID); err != nil {
					return err
				}
			}
			switch disposition {
			case DispositionTarget, DispositionBoth:
				// targetContainerID already holds it.
			default:
				targetContainerID = nil
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_date, expiration_source, container_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at, container_id`,
			newBatchID, source.ProductID, in.TargetLocationID, in.Quantity,
			source.ExpirationDate, string(source.ExpirationSource), targetContainerID)

		created, err := scanBatch(row)
		if err != nil {
			return err
		}

		if err := writeLog(ctx, tx, source.ProductID, &batchID, -in.Quantity, ReasonMove, userID); err != nil {
			return err
		}
		if err := writeLog(ctx, tx, source.ProductID, &created.ID, in.Quantity, ReasonMove, userID); err != nil {
			return err
		}

		// Re-read through the joining path so the created batch carries its
		// container's label, not just an id the caller would have to resolve.
		out, err = loadBatch(ctx, tx, storageID, created.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BatchPatch is a partial update to one batch: the whole-batch move of
// docs/specs/06-vision-shelf-ingestion.md, the manual quantity correction of
// docs/specs/13-stocktake-and-audit.md, or both at once.
//
// A nil field is "leave this alone". Neither field set is a request that
// cannot be carried out, and is refused rather than answered 200 for a write
// that never happened.
//
// Both in one call have to land together or not at all. Two sequential
// transactions would leave a corrected quantity committed and a move lost —
// a half-applied PATCH the caller cannot detect and the server cannot undo,
// the same failure UpdateLocation's single transaction exists to prevent.
type BatchPatch struct {
	// Quantity is the batch's new absolute quantity, not a delta. Zero
	// deletes the batch, exactly as a consumption reaching zero does.
	Quantity *int
	// LocationID is the shelf the whole batch moves to.
	LocationID *uuid.UUID

	// SetContainerLabel says the caller named container_label at all, which is
	// the difference between "leave the container alone" and "this batch is in
	// nothing" — a distinction a bare pointer cannot carry, and the same shape
	// ProductPatch uses for its own nullable fields.
	SetContainerLabel bool
	// ContainerLabel is the label to upsert: a new container on the first set,
	// a rename of the same row afterwards. Nil with SetContainerLabel means
	// detach without destroying the container.
	ContainerLabel *string

	// SetContainerType says the caller named container_type at all.
	SetContainerType bool
	// ContainerType is the descriptive kind ("box", "bag"), or nil to clear it.
	// Either way it needs a container to write to — an existing one or one this
	// same patch creates — and is ErrValidation without.
	ContainerType *string
}

// touchesContainer reports whether this patch says anything about the batch's
// container.
func (p BatchPatch) touchesContainer() bool {
	return p.SetContainerLabel || p.SetContainerType
}

// UpdateBatch applies a BatchPatch in one transaction and returns the batch as
// it stands afterwards — or nil when the patch set the quantity to zero and so
// deleted it.
//
// This is the only exported way to move a batch or set its quantity. One
// public write path rather than several is what keeps the ledger invariant
// enforceable: every branch below either goes through adjustBatch or writes
// its own paired log rows, and there is no second entry point where a future
// change could quietly skip that.
//
// Refusals:
//   - ErrNotFound for a batch or a target location that is not in this
//     storage — the same answer as one that does not exist.
//   - ErrValidation for an empty patch, a negative quantity, or a patch that
//     asks to empty a batch and move it in the same breath.
func (s *Store) UpdateBatch(ctx context.Context, storageID, batchID uuid.UUID, patch BatchPatch, userID *uuid.UUID) (*Batch, error) {
	if patch.Quantity == nil && patch.LocationID == nil && !patch.touchesContainer() {
		return nil, fmt.Errorf("%w: a batch patch must name at least one field", ErrValidation)
	}
	// "The shelf is empty" and "carry it to the kitchen" contradict each
	// other, and applying them in either order gives a different answer.
	// Refusing is the only reading that cannot silently pick one.
	if patch.Quantity != nil && *patch.Quantity == 0 && patch.LocationID != nil {
		return nil, fmt.Errorf("%w: a batch set to zero is deleted, so it cannot also be moved", ErrValidation)
	}
	// Same contradiction, same refusal: a batch emptied to zero is deleted in
	// this transaction, so there is no row left for a container to be attached
	// to, renamed on, or cleared from.
	if patch.Quantity != nil && *patch.Quantity == 0 && patch.touchesContainer() {
		return nil, fmt.Errorf("%w: a batch set to zero is deleted, so its container cannot also be changed", ErrValidation)
	}

	var out *Batch
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if patch.Quantity != nil {
			if _, _, err := setBatchQuantity(ctx, tx, storageID, batchID, *patch.Quantity, userID); err != nil {
				return err
			}
			if *patch.Quantity == 0 {
				// The row is gone, so there is nothing to return and nothing
				// left for a move to act on.
				out = nil
				return nil
			}
		}

		// The container fields come before the move only so that the single
		// loadBatch at the end can serve every branch; neither ordering is
		// observable, because a container write touches no quantity and a move
		// touches no container (docs/specs/39-batch-containers.md: an entire
		// batch moving carries its container with it either way).
		//
		// The batch has not been resolved against the storage yet on a patch
		// that names only container fields, so that happens first — a batch id
		// from another storage must be the same ErrNotFound an unknown one is,
		// before any container row is created.
		if patch.touchesContainer() {
			if _, err := loadBatch(ctx, tx, storageID, batchID); err != nil {
				return err
			}
			if patch.SetContainerLabel {
				if patch.ContainerLabel == nil {
					if err := detachBatchContainer(ctx, tx, batchID); err != nil {
						return err
					}
				} else if err := upsertBatchContainerLabel(ctx, tx, storageID, batchID, *patch.ContainerLabel); err != nil {
					return err
				}
			}
			if patch.SetContainerType {
				if err := setBatchContainerType(ctx, tx, storageID, batchID, patch.ContainerType); err != nil {
					return err
				}
			}
		}

		if patch.LocationID != nil {
			if _, err := moveBatch(ctx, tx, storageID, batchID, *patch.LocationID, userID); err != nil {
				return err
			}
		}

		batch, err := loadBatch(ctx, tx, storageID, batchID)
		if err != nil {
			return err
		}
		out = batch
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// setBatchQuantity sets a batch's absolute quantity inside a caller's
// transaction, reporting whether anything actually changed.
//
// It is the single place both correction paths of
// docs/specs/13-stocktake-and-audit.md meet — the one-off PATCH and the
// guided stocktake — so "exactly one 'audit' row per changed quantity, and
// none at all for an unchanged one" is a property of one function rather than
// a rule two call sites have to remember.
//
// The unchanged case writes nothing whatsoever, not a zero-delta row: the
// ledger explains changes, and "the count was already right" is not one.
// That is also why this cannot simply hand the computed delta to adjustBatch
// and ignore the outcome — adjustBatch refuses a zero delta for exactly that
// reason, and swallowing the refusal would hide real ones too.
func setBatchQuantity(ctx context.Context, tx pgx.Tx, storageID, batchID uuid.UUID, quantity int, userID *uuid.UUID) (productID uuid.UUID, changed bool, err error) {
	if quantity < 0 {
		return uuid.Nil, false, fmt.Errorf("%w: batch quantity must be zero or more, got %d", ErrValidation, quantity)
	}

	var current int
	err = tx.QueryRow(ctx, `
		SELECT b.product_id, b.quantity
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE b.id = $1 AND p.storage_id = $2
		 FOR UPDATE OF b`, batchID, storageID).Scan(&productID, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("store: load batch to count: %w", err)
	}

	if quantity == current {
		return productID, false, nil
	}

	// adjustBatch owns delete-on-zero and the paired log row; the only thing
	// this function contributes is the absolute-to-delta conversion and the
	// decision not to call it at all.
	if _, err := adjustBatch(ctx, tx, storageID, batchID, quantity-current, ReasonAudit, userID); err != nil {
		return uuid.Nil, false, err
	}
	return productID, true, nil
}

// moveBatch relocates an entire batch inside a caller's transaction — the
// whole-batch counterpart to SplitBatch (docs/specs/06-vision-shelf-ingestion.md).
//
// Moving everything is deliberately not a split: SplitBatch refuses a quantity
// equal to the whole batch, because the result would be an emptied row the
// model says should not exist. Here the row keeps its identity — same id, same
// created_at, same expiry, same provenance — and only its shelf changes.
//
// The paired 'move' log rows are written as the spec requires, and it is worth
// being honest about what they can and cannot tell you afterwards:
// inventory_logs has no location column, so a log's location is whatever its
// batch points at *now*. For a split that is exact, because the two halves are
// separate rows at separate locations. For a whole-batch move both rows resolve
// to the destination, so the pair records that a move happened, when, and by
// whom — not a per-location before-and-after. Reconstructing that would need a
// location column on the ledger, which is a data-model change and belongs to
// docs/specs/02-data-model.md, not here.
func moveBatch(ctx context.Context, tx pgx.Tx, storageID, batchID, targetLocationID uuid.UUID, userID *uuid.UUID) (*Batch, error) {
	var productID, currentLocation uuid.UUID
	var quantity int
	err := tx.QueryRow(ctx, `
		SELECT b.product_id, b.location_id, b.quantity
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE b.id = $1 AND p.storage_id = $2
		 FOR UPDATE OF b`, batchID, storageID).Scan(&productID, &currentLocation, &quantity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: load batch to move: %w", err)
	}

	if err := requireSameStorage(ctx, tx, treeLocations, storageID, targetLocationID); err != nil {
		return nil, err
	}

	// Moving a batch to the shelf it is already on is a no-op, not an
	// error: PATCH with the current value has to succeed, or a client that
	// resends its own state gets a failure for changing nothing. The two
	// log rows are skipped because they would explain nothing — the same
	// reason AdjustBatch refuses a zero delta.
	if currentLocation == targetLocationID {
		return loadBatch(ctx, tx, storageID, batchID)
	}

	row := tx.QueryRow(ctx, `
		UPDATE inventory_batches SET location_id = $1 WHERE id = $2
		RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at, container_id`,
		targetLocationID, batchID)

	moved, err := scanBatch(row)
	if err != nil {
		return nil, err
	}

	if err := writeLog(ctx, tx, productID, &batchID, -quantity, ReasonMove, userID); err != nil {
		return nil, err
	}
	if err := writeLog(ctx, tx, productID, &batchID, quantity, ReasonMove, userID); err != nil {
		return nil, err
	}

	return moved, nil
}

// loadBatch reads one batch scoped to a storage, with its container's label and
// type joined in. A batch belonging to another storage is ErrNotFound, the same
// answer as one that does not exist.
//
// This is the path every write returns through, so a caller that just set a
// container label reads it back rather than an id it would have to resolve.
func loadBatch(ctx context.Context, q querier, storageID, batchID uuid.UUID) (*Batch, error) {
	return scanBatchWithContainer(q.QueryRow(ctx, `
		SELECT b.id, b.product_id, b.location_id, b.quantity, b.expiration_date,
		       b.expiration_source, b.created_at, b.container_id, c.label, c.container_type
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		  LEFT JOIN containers c ON c.id = b.container_id
		 WHERE b.id = $1 AND p.storage_id = $2`, batchID, storageID))
}

// writeLog appends the inventory_logs row that explains a quantity change.
//
// It is unexported and takes a transaction because of the rule in
// docs/specs/02-data-model.md: every write to inventory_batches.quantity must
// be paired, in the same transaction, with a log row. Keeping this private
// means no caller outside this file can move stock without also explaining it.
func writeLog(ctx context.Context, tx pgx.Tx, productID uuid.UUID, batchID *uuid.UUID, changeQty int, reason LogReason, userID *uuid.UUID) error {
	id, err := newID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory_logs (id, product_id, batch_id, change_qty, reason, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, productID, batchID, changeQty, string(reason), userID); err != nil {
		return fmt.Errorf("store: write inventory log: %w", err)
	}
	return nil
}

// scanBatch reads the eight inventory_batches columns, container_id last. It is
// the scanner for the write paths — INSERT/UPDATE ... RETURNING cannot join
// containers — so the Batch it returns carries a container id and no label.
func scanBatch(row rowScanner) (*Batch, error) {
	var b Batch
	var source string
	err := row.Scan(&b.ID, &b.ProductID, &b.LocationID, &b.Quantity, &b.ExpirationDate,
		&source, &b.CreatedAt, &b.ContainerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan batch: %w", err)
	}
	b.ExpirationSource = ExpirationSource(source)
	return &b, nil
}

// scanBatchWithContainer reads scanBatch's columns plus the joined
// containers.label and containers.container_type, in that order. Both are NULL
// for a batch with no container, which is why they are pointers rather than a
// separate presence flag.
func scanBatchWithContainer(row rowScanner) (*Batch, error) {
	var b Batch
	var source string
	err := row.Scan(&b.ID, &b.ProductID, &b.LocationID, &b.Quantity, &b.ExpirationDate,
		&source, &b.CreatedAt, &b.ContainerID, &b.ContainerLabel, &b.ContainerType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan batch with container: %w", err)
	}
	b.ExpirationSource = ExpirationSource(source)
	return &b, nil
}
