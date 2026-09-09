package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

	var out *Batch
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireProductInStorage(ctx, tx, storageID, in.ProductID); err != nil {
			return err
		}
		if err := requireSameStorage(ctx, tx, treeLocations, storageID, in.LocationID); err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO inventory_batches (id, product_id, location_id, quantity, expiration_date, expiration_source)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at`,
			id, in.ProductID, in.LocationID, in.Quantity, in.ExpirationDate, string(in.ExpirationSource))

		batch, err := scanBatch(row)
		if err != nil {
			return err
		}

		if err := writeLog(ctx, tx, in.ProductID, &batch.ID, in.Quantity, in.Reason, in.CreatedBy); err != nil {
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
		var productID uuid.UUID
		var quantity int
		err := tx.QueryRow(ctx, `
			SELECT b.product_id, b.quantity
			  FROM inventory_batches b
			  JOIN products p ON p.id = b.product_id
			 WHERE b.id = $1 AND p.storage_id = $2
			 FOR UPDATE OF b`, batchID, storageID).Scan(&productID, &quantity)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: load batch: %w", err)
		}

		updated := quantity + delta
		if updated < 0 {
			return fmt.Errorf("%w: batch holds %d, cannot apply %d", ErrValidation, quantity, delta)
		}

		if updated == 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM inventory_batches WHERE id = $1`, batchID); err != nil {
				return fmt.Errorf("store: delete emptied batch: %w", err)
			}
			// batch_id is ON DELETE SET NULL, so the log keeps its meaning
			// after the row it pointed at is gone.
			return writeLog(ctx, tx, productID, nil, delta, reason, userID)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE inventory_batches SET quantity = $1 WHERE id = $2`, updated, batchID); err != nil {
			return fmt.Errorf("store: update batch quantity: %w", err)
		}
		return writeLog(ctx, tx, productID, &batchID, delta, reason, userID)
	})
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
