package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// inTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic.
//
// Several invariants in docs/specs/02-data-model.md are only true as
// transactions — most importantly that every write to
// inventory_batches.quantity is paired with an inventory_logs row, and that a
// tombstone is written in the same transaction as the delete it records.
// Routing those writes through one helper is what keeps the pairing from
// depending on each caller remembering it.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}

	defer func() {
		// Rollback after a successful commit is a no-op; this only matters on
		// the error and panic paths, where it is what prevents a leaked
		// connection sitting in an open transaction.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// newID returns a UUIDv7. Ids are generated in Go rather than by the database
// so a multi-row write knows its own keys before it starts
// (docs/specs/02-data-model.md).
func newID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: generate id: %w", err)
	}
	return id, nil
}
