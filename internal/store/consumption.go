package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ConsumeDecision is the reviewer's verdict on one proposed consumption row
// (docs/specs/09-consumption-logging.md).
type ConsumeDecision struct {
	RowID  string
	Accept bool

	// ProductID is required on an accepted row. Consumption never creates a
	// product — unlike IngestDecision there is no NewProduct alternative.
	ProductID *uuid.UUID

	// Decrements names which existing batch(es) the row's decrement applies
	// to. At least one is required on an accepted row; several let a
	// reviewer split a decrement across locations.
	Decrements []ConsumeBatchDecrement
}

// ConsumeBatchDecrement is one batch a row's decrement removes units from.
type ConsumeBatchDecrement struct {
	BatchID  uuid.UUID
	Quantity int
}

// ConsumeResult is what a confirm wrote.
type ConsumeResult struct {
	// BatchIDs are the batches touched, whether they were emptied and deleted
	// or only reduced.
	BatchIDs []uuid.UUID
}

// ConfirmConsumption applies a reviewed consumption proposal, all or nothing.
//
// It mirrors ConfirmIngestion — one transaction, the same row_id exactness
// check, the job marked consumed last — but the direction is reversed: an
// accepted row decrements batches this storage already has rather than
// creating one. A batch reaching exactly zero is deleted, the same rule
// AdjustBatch already applies, but the product itself is never touched, so a
// fully consumed product stays browsable at zero stock
// (docs/specs/10-reorder-and-shopping-export.md).
//
// Refusals:
//   - ErrNotFound for a job, product or batch that is not in this storage —
//     the same answer as one that does not exist.
//   - ErrConflict for a job that is not done: still pending, failed, or
//     already consumed by an earlier confirm.
//   - ErrValidation when the decisions do not name exactly the rows the
//     proposal issued, when an accepted row has no product or no decrements,
//     when a decrement would take its batch below zero, or when a decrement
//     names a batch that does not belong to the row's product.
func (s *Store) ConfirmConsumption(ctx context.Context, storageID, jobID uuid.UUID, userID *uuid.UUID, decisions []ConsumeDecision) (*ConsumeResult, error) {
	result := &ConsumeResult{BatchIDs: []uuid.UUID{}}

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var status JobStatus
		var payload []byte
		err := tx.QueryRow(ctx, `
			SELECT status, payload FROM jobs WHERE id = $1 AND storage_id = $2 FOR UPDATE`,
			jobID, storageID).Scan(&status, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock consumption job: %w", err)
		}
		if status != JobDone {
			return fmt.Errorf("%w: job is %s, not done", ErrConflict, status)
		}

		ids := make([]string, len(decisions))
		for i, d := range decisions {
			ids[i] = d.RowID
		}
		if err := matchRowIDs(payload, ids); err != nil {
			return err
		}

		for _, d := range decisions {
			if !d.Accept {
				continue
			}
			if d.ProductID == nil {
				return fmt.Errorf("%w: row %s has no product", ErrValidation, d.RowID)
			}
			if err := requireProductInStorage(ctx, tx, storageID, *d.ProductID); err != nil {
				return err
			}
			if len(d.Decrements) == 0 {
				return fmt.Errorf("%w: row %s has no decrements", ErrValidation, d.RowID)
			}

			for _, dec := range d.Decrements {
				productID, err := adjustBatch(ctx, tx, storageID, dec.BatchID, -dec.Quantity, ReasonConsumption, userID)
				if err != nil {
					return err
				}
				if productID != *d.ProductID {
					return fmt.Errorf("%w: batch %s does not belong to row %s's product", ErrValidation, dec.BatchID, d.RowID)
				}
				result.BatchIDs = append(result.BatchIDs, dec.BatchID)
			}
		}

		return ConsumeJob(ctx, tx, storageID, jobID)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
