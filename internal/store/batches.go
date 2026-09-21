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

const (
	ExpirationDerived ExpirationSource = "derived"
	ExpirationUser    ExpirationSource = "user"
)

// Batch is a quantity of one product at exactly one location.
type Batch struct {
	ID               uuid.UUID
	ProductID        uuid.UUID
	LocationID       uuid.UUID
	Quantity         int
	ExpirationDate   *time.Time
	ExpirationSource ExpirationSource
	CreatedAt        time.Time
}

// NewBatch is the input to CreateBatch.
type NewBatch struct {
	ProductID        uuid.UUID
	LocationID       uuid.UUID
	Quantity         int
	ExpirationDate   *time.Time
	ExpirationSource ExpirationSource
	Reason           LogReason
	CreatedBy        *uuid.UUID
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
		RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at`,
		id, in.ProductID, in.LocationID, in.Quantity, in.ExpirationDate, string(in.ExpirationSource))

	batch, err := scanBatch(row)
	if err != nil {
		return nil, err
	}

	if err := writeLog(ctx, tx, in.ProductID, &batch.ID, in.Quantity, in.Reason, in.CreatedBy); err != nil {
		return nil, err
	}
	if err := bumpForLedgerReason(ctx, tx, storageID, in.CreatedBy, in.Reason, batch.ID); err != nil {
		return nil, err
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

	rows, err := s.pool.Query(ctx, `
		SELECT id, product_id, location_id, quantity, expiration_date, expiration_source, created_at
		  FROM inventory_batches
		 WHERE product_id = $1
		 ORDER BY expiration_date NULLS LAST, created_at`, productID)
	if err != nil {
		return nil, fmt.Errorf("store: list product batches: %w", err)
	}
	defer rows.Close()

	out := []Batch{}
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
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
func (s *Store) SplitBatch(ctx context.Context, storageID, batchID uuid.UUID, quantity int, targetLocationID uuid.UUID, userID *uuid.UUID) (*Batch, error) {
	newBatchID, err := newID()
	if err != nil {
		return nil, err
	}

	var out *Batch
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var source Batch
		var srcExpiration string
		err := tx.QueryRow(ctx, `
			SELECT b.id, b.product_id, b.location_id, b.quantity, b.expiration_date, b.expiration_source
			  FROM inventory_batches b
			  JOIN products p ON p.id = b.product_id
			 WHERE b.id = $1 AND p.storage_id = $2
			 FOR UPDATE OF b`, batchID, storageID).
			Scan(&source.ID, &source.ProductID, &source.LocationID, &source.Quantity,
				&source.ExpirationDate, &srcExpiration)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: load batch to split: %w", err)
		}
		source.ExpirationSource = ExpirationSource(srcExpiration)

		// Splitting the whole batch is a move, which is a different operation.
		if quantity < 1 || quantity > source.Quantity-1 {
			return fmt.Errorf("%w: split quantity must be between 1 and %d", ErrValidation, source.Quantity-1)
		}
		if err := requireSameStorage(ctx, tx, treeLocations, storageID, targetLocationID); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE inventory_batches SET quantity = quantity - $1 WHERE id = $2`,
			quantity, batchID); err != nil {
			return fmt.Errorf("store: debit source batch: %w", err)
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_date, expiration_source)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at`,
			newBatchID, source.ProductID, targetLocationID, quantity,
			source.ExpirationDate, string(source.ExpirationSource))

		created, err := scanBatch(row)
		if err != nil {
			return err
		}

		if err := writeLog(ctx, tx, source.ProductID, &batchID, -quantity, ReasonMove, userID); err != nil {
			return err
		}
		if err := writeLog(ctx, tx, source.ProductID, &created.ID, quantity, ReasonMove, userID); err != nil {
			return err
		}

		out = created
		return nil
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
	if patch.Quantity == nil && patch.LocationID == nil {
		return nil, fmt.Errorf("%w: a batch patch must name at least one field", ErrValidation)
	}
	// "The shelf is empty" and "carry it to the kitchen" contradict each
	// other, and applying them in either order gives a different answer.
	// Refusing is the only reading that cannot silently pick one.
	if patch.Quantity != nil && *patch.Quantity == 0 && patch.LocationID != nil {
		return nil, fmt.Errorf("%w: a batch set to zero is deleted, so it cannot also be moved", ErrValidation)
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

		if patch.LocationID != nil {
			moved, err := moveBatch(ctx, tx, storageID, batchID, *patch.LocationID, userID)
			if err != nil {
				return err
			}
			out = moved
			return nil
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
		RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at`,
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

// loadBatch reads one batch scoped to a storage. A batch belonging to another
// storage is ErrNotFound, the same answer as one that does not exist.
func loadBatch(ctx context.Context, q querier, storageID, batchID uuid.UUID) (*Batch, error) {
	return scanBatch(q.QueryRow(ctx, `
		SELECT b.id, b.product_id, b.location_id, b.quantity, b.expiration_date, b.expiration_source, b.created_at
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
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

func scanBatch(row rowScanner) (*Batch, error) {
	var b Batch
	var source string
	err := row.Scan(&b.ID, &b.ProductID, &b.LocationID, &b.Quantity, &b.ExpirationDate, &source, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan batch: %w", err)
	}
	b.ExpirationSource = ExpirationSource(source)
	return &b, nil
}
