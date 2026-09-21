package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StocktakeSheet is one location's shelf as the database believes it to be —
// the thing a person stands in front of and disagrees with
// (docs/specs/13-stocktake-and-audit.md).
type StocktakeSheet struct {
	Location Location
	Batches  []StocktakeBatch
}

// StocktakeBatch is one row of the sheet: a batch, plus the two product
// fields needed to recognise it on a shelf without a second request.
type StocktakeBatch struct {
	Batch
	ProductName string
	ImageURL    *string
}

// StocktakeCount is the counted quantity for one batch the location holds.
// Quantity is absolute, not a delta — a person counts jars, they do not
// compute differences.
type StocktakeCount struct {
	BatchID  uuid.UUID
	Quantity int
}

// FoundStock is stock on the shelf that the database had no batch for.
type FoundStock struct {
	ProductID uuid.UUID
	Quantity  int
	// ExpirationDate is the date the caller stated, if any.
	ExpirationDate *time.Time
	// StatedExpiration separates "the caller said when this expires —
	// possibly that it does not" from "the caller said nothing". Only the
	// second resolves the shelf-life rules
	// (docs/specs/08-expiration-and-classification.md); the first is a
	// deliberate statement and is stored with expiration_source 'user',
	// which the expiry cascade must never overwrite.
	StatedExpiration bool
}

// StocktakeResult is what a confirmed walk wrote.
type StocktakeResult struct {
	// AuditedAt is the timestamp the location now carries. It is set even
	// when nothing else changed: "I checked and it was right" is precisely
	// the fact the column records.
	AuditedAt time.Time
	// Corrected counts the batches whose quantity actually moved, so a caller
	// can tell "the shelf was right" from "eleven things were wrong".
	Corrected int
	// CreatedBatchIDs are the batches found stock created.
	CreatedBatchIDs []uuid.UUID
}

// LocationStocktake reads the sheet for one location.
//
// Only batches sitting *directly* at the node are returned. A stocktake
// mirrors one physical shelf, and a child location is its own walk — folding
// descendants in would ask a person to count things they are not looking at.
//
// A location in another storage is ErrNotFound, indistinguishable from one
// that never existed (docs/specs/03-auth-and-multi-tenancy.md).
func (s *Store) LocationStocktake(ctx context.Context, storageID, locationID uuid.UUID) (*StocktakeSheet, error) {
	location, err := scanLocation(s.pool.QueryRow(ctx, `
		SELECT id, storage_id, parent_id, name, description, created_at, updated_at, last_audited_at
		  FROM locations
		 WHERE id = $1 AND storage_id = $2`, locationID, storageID))
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.product_id, b.location_id, b.quantity, b.expiration_date,
		       b.expiration_source, b.created_at, p.name, p.image_url
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE b.location_id = $1 AND p.storage_id = $2
		 ORDER BY p.name, b.expiration_date NULLS LAST, b.created_at`, locationID, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: list stocktake batches: %w", err)
	}
	defer rows.Close()

	sheet := &StocktakeSheet{Location: *location, Batches: []StocktakeBatch{}}
	for rows.Next() {
		var b StocktakeBatch
		var source string
		if err := rows.Scan(&b.ID, &b.ProductID, &b.LocationID, &b.Quantity, &b.ExpirationDate,
			&source, &b.CreatedAt, &b.ProductName, &b.ImageURL); err != nil {
			return nil, fmt.Errorf("store: scan stocktake batch: %w", err)
		}
		b.ExpirationSource = ExpirationSource(source)
		sheet.Batches = append(sheet.Batches, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list stocktake batches: %w", err)
	}
	return sheet, nil
}

// ConfirmStocktake applies a walked location's counts and its found stock, all
// or nothing (docs/specs/13-stocktake-and-audit.md).
//
// The counts must name exactly the batches the location currently holds — the
// same exact-row-set rule ConfirmConsumption applies to a proposal's rows
// (docs/specs/09-consumption-logging.md). A missing or unknown id is
// ErrValidation and the whole call writes nothing. Guessing would be
// last-write-wins on a flow whose entire purpose is correctness: if another
// member moved something since the sheet was fetched, the honest answer is
// "re-read the shelf", not "apply what you have and lose theirs".
//
// Refusals:
//   - ErrNotFound for the location, or for a found entry's product, when it
//     is not in this storage.
//   - ErrValidation when the counted batch ids are not exactly the ones the
//     location holds, when a count is negative, or when a found quantity is
//     below one.
func (s *Store) ConfirmStocktake(ctx context.Context, storageID, locationID uuid.UUID, userID *uuid.UUID, counts []StocktakeCount, found []FoundStock) (*StocktakeResult, error) {
	result := &StocktakeResult{CreatedBatchIDs: []uuid.UUID{}}

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireSameStorage(ctx, tx, treeLocations, storageID, locationID); err != nil {
			return err
		}

		held, err := lockLocationBatches(ctx, tx, storageID, locationID)
		if err != nil {
			return err
		}
		if err := matchBatchIDs(held, counts); err != nil {
			return err
		}

		for _, c := range counts {
			_, changed, err := setBatchQuantity(ctx, tx, storageID, c.BatchID, c.Quantity, userID)
			if err != nil {
				return err
			}
			if changed {
				result.Corrected++
			}
		}

		for _, f := range found {
			source := ExpirationDerived
			if f.StatedExpiration {
				source = ExpirationUser
			}
			// 'audit', not 'purchase': nothing was bought in this moment, the
			// record is being corrected to match a shelf that already held
			// this. Turnover analytics count purchases and consumption
			// (docs/specs/11-reporting-and-analytics.md), and a correction is
			// neither.
			batch, err := createBatch(ctx, tx, storageID, NewBatch{
				ProductID:        f.ProductID,
				LocationID:       locationID,
				Quantity:         f.Quantity,
				ExpirationDate:   f.ExpirationDate,
				ExpirationSource: source,
				Reason:           ReasonAudit,
				CreatedBy:        userID,
			})
			if err != nil {
				return err
			}
			result.CreatedBatchIDs = append(result.CreatedBatchIDs, batch.ID)
		}

		// updated_at moves with last_audited_at. A client's delta sync asks
		// "everything changed since X" (docs/specs/12-client-api-contract.md),
		// and a node whose audit timestamp changed without it would keep
		// serving the stale "never audited" from cache indefinitely, with
		// nothing anywhere to notice.
		if err := tx.QueryRow(ctx, `
			UPDATE locations SET last_audited_at = now(), updated_at = now()
			 WHERE id = $1 AND storage_id = $2
			RETURNING last_audited_at`, locationID, storageID).Scan(&result.AuditedAt); err != nil {
			return fmt.Errorf("store: record stocktake: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// lockLocationBatches returns the ids of the batches directly at a location,
// locking those rows for the rest of the transaction.
//
// The lock is what stops two confirms of the same shelf from both passing the
// exactness check and then applying counts computed from different starting
// quantities. It does not — and cannot, without predicate locking — stop a
// concurrent *insert* at the same location; that case is caught the other way
// round, when the inserting session's own confirm finds a batch it did not
// count.
func lockLocationBatches(ctx context.Context, tx pgx.Tx, storageID, locationID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT b.id
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE b.location_id = $1 AND p.storage_id = $2
		 ORDER BY b.id
		 FOR UPDATE OF b`, locationID, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: lock location batches: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: lock location batches: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: lock location batches: %w", err)
	}
	return ids, nil
}

// matchBatchIDs holds a confirm to the exact row set the location currently
// holds, the same shape matchRowIDs applies to a review job's proposal.
//
// A batch id that is not among them is ErrValidation whatever the reason —
// it never existed, it belongs to another storage, or it sits on a different
// shelf in this one. That is deliberately not the 404 the same foreign id
// would get from a route that addresses it directly: here the id is not being
// resolved, it is being compared against a set, and all three cases produce
// the identical error, so there is nothing for a prober to tell apart
// (docs/specs/03-auth-and-multi-tenancy.md).
func matchBatchIDs(held []uuid.UUID, counts []StocktakeCount) error {
	current := make(map[uuid.UUID]bool, len(held))
	for _, id := range held {
		current[id] = true
	}

	seen := make(map[uuid.UUID]bool, len(counts))
	for _, c := range counts {
		if !current[c.BatchID] {
			return fmt.Errorf("%w: batch %s is not at this location", ErrValidation, c.BatchID)
		}
		if seen[c.BatchID] {
			return fmt.Errorf("%w: batch %s is counted twice", ErrValidation, c.BatchID)
		}
		seen[c.BatchID] = true
	}
	if len(seen) != len(current) {
		return fmt.Errorf("%w: every batch at this location needs a count; %d of %d were counted",
			ErrValidation, len(seen), len(current))
	}
	return nil
}
