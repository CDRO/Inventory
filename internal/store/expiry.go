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

// The database side of docs/specs/08-expiration-and-classification.md: gather
// the rules that apply to a product, and recompute the dates that follow from
// them.
//
// The arithmetic itself lives in internal/expiry, which has no database types
// in it. This file's job is only to fetch the inputs in the right order and to
// be extremely careful about which rows it is allowed to write.

// ExpiryRulesFor gathers every rule that applies to one product, in the
// priority order internal/expiry expects.
//
// The category walk climbs from the product's own category toward the root,
// bounded by maxTreeDepth for the same reason the tree helpers are: a cycle in
// already-corrupt data must not turn this into an unbounded query.
func (s *Store) ExpiryRulesFor(ctx context.Context, storageID, productID uuid.UUID) (expiry.Rules, error) {
	return expiryRulesFor(ctx, s.pool, storageID, productID)
}

func expiryRulesFor(ctx context.Context, q querier, storageID, productID uuid.UUID) (expiry.Rules, error) {
	var rules expiry.Rules
	var categoryID *uuid.UUID
	var itemType string

	err := q.QueryRow(ctx, `
		SELECT p.default_shelf_life_days,
		       c.default_shelf_life_days,
		       p.category_id,
		       p.item_type
		  FROM products p
		  LEFT JOIN catalog_products c ON c.id = p.catalog_id
		 WHERE p.id = $1 AND p.storage_id = $2`,
		productID, storageID).Scan(&rules.ProductDays, &rules.CatalogDays, &categoryID, &itemType)
	if errors.Is(err, pgx.ErrNoRows) {
		return expiry.Rules{}, ErrNotFound
	}
	if err != nil {
		return expiry.Rules{}, fmt.Errorf("store: load expiry rules: %w", err)
	}
	rules.ItemType = expiry.ItemType(itemType)

	if categoryID == nil {
		return rules, nil
	}

	// Own category first, then each ancestor. depth orders the walk so the
	// nearest rule wins, which is what makes "Food → Dairy: 10" override
	// "Food: 365" for cheese.
	rows, err := q.Query(ctx, fmt.Sprintf(`
		WITH RECURSIVE ancestry AS (
		    SELECT id, parent_id, default_shelf_life_days, 1 AS depth
		      FROM categories
		     WHERE id = $1 AND storage_id = $2
		    UNION ALL
		    SELECT c.id, c.parent_id, c.default_shelf_life_days, a.depth + 1
		      FROM categories c
		      JOIN ancestry a ON c.id = a.parent_id
		     WHERE a.depth < %d
		)
		SELECT default_shelf_life_days FROM ancestry ORDER BY depth`, maxTreeDepth),
		*categoryID, storageID)
	if err != nil {
		return expiry.Rules{}, fmt.Errorf("store: walk category ancestry: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var days *int
		if err := rows.Scan(&days); err != nil {
			return expiry.Rules{}, fmt.Errorf("store: scan category shelf life: %w", err)
		}
		rules.CategoryDays = append(rules.CategoryDays, days)
	}
	if err := rows.Err(); err != nil {
		return expiry.Rules{}, fmt.Errorf("store: walk category ancestry: %w", err)
	}

	return rules, nil
}

// ResolveExpiryFor returns the date a batch of this product, created at
// createdAt, should carry.
func (s *Store) ResolveExpiryFor(ctx context.Context, storageID, productID uuid.UUID, createdAt time.Time) (*time.Time, expiry.Source, error) {
	rules, err := s.ExpiryRulesFor(ctx, storageID, productID)
	if err != nil {
		return nil, "", err
	}
	resolution := expiry.Resolve(rules)
	return expiry.DateFor(createdAt, resolution), resolution.Source, nil
}

// RecomputeDerivedExpiry recomputes expiration_date for a product's existing
// batches and returns how many rows changed.
//
// **It touches only rows with expiration_source = 'derived'.** That single
// WHERE clause is the whole guarantee of
// docs/specs/08-expiration-and-classification.md: a date a person typed —
// including the deliberate "no expiry at all", which is a NULL date with a
// 'user' source — outranks every rule here, forever. Putting the condition in
// the statement rather than in a loop above it means no caller can forget it
// and no future refactor can drop it without deleting the line.
//
// No inventory_logs rows are written: quantities do not change, and a ledger
// entry recording "nothing moved" would be noise in the one table that exists
// to explain movement.
func (s *Store) RecomputeDerivedExpiry(ctx context.Context, storageID, productID uuid.UUID) (int, error) {
	var affected int

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		n, err := recomputeDerivedExpiry(ctx, tx, storageID, productID)
		affected = n
		return err
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// recomputeDerivedExpiry is the transaction-scoped body.
//
// It exists so that a caller already inside a transaction — re-filing a
// product into a different category, say — recomputes in the *same*
// transaction as the change that caused it. Opening a second one would leave a
// window where the product's category and its batches' dates disagree, and a
// failure in between would make that permanent.
func recomputeDerivedExpiry(ctx context.Context, tx pgx.Tx, storageID, productID uuid.UUID) (int, error) {
	affected := 0

	err := func() error {
		rules, err := expiryRulesFor(ctx, tx, storageID, productID)
		if err != nil {
			return err
		}
		resolution := expiry.Resolve(rules)

		// Recomputed per batch, because the date is relative to when that
		// batch was created: two batches of the same product added a week
		// apart do not expire on the same day.
		rows, err := tx.Query(ctx, `
			SELECT b.id, b.created_at
			  FROM inventory_batches b
			 WHERE b.product_id = $1 AND b.expiration_source = 'derived'
			 FOR UPDATE`, productID)
		if err != nil {
			return fmt.Errorf("store: load derived batches: %w", err)
		}

		type batchRow struct {
			id        uuid.UUID
			createdAt time.Time
		}
		var batches []batchRow
		for rows.Next() {
			var b batchRow
			if err := rows.Scan(&b.id, &b.createdAt); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan derived batch: %w", err)
			}
			batches = append(batches, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: load derived batches: %w", err)
		}

		for _, b := range batches {
			date := expiry.DateFor(b.createdAt, resolution)

			// The source is reasserted rather than left alone: these rows are
			// derived by definition, and writing it keeps the column honest if
			// one ever got there another way.
			tag, err := tx.Exec(ctx, `
				UPDATE inventory_batches
				   SET expiration_date = $1, expiration_source = 'derived'
				 WHERE id = $2 AND expiration_source = 'derived'`, date, b.id)
			if err != nil {
				return fmt.Errorf("store: recompute batch expiry: %w", err)
			}
			affected += int(tag.RowsAffected())
		}
		return nil
	}()
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// RecomputeDerivedExpiryForCategory recomputes every product in one storage
// that resolves through a category, after that category's rule changed.
//
// The subtree walk matters: changing "Food" has to reach a product filed under
// "Food → Dairy → Cheese", because that product's chain climbs through the row
// that just changed.
func (s *Store) RecomputeDerivedExpiryForCategory(ctx context.Context, storageID, categoryID uuid.UUID) (int, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		WITH RECURSIVE subtree AS (
		    SELECT id, 1 AS depth FROM categories WHERE id = $1 AND storage_id = $2
		    UNION ALL
		    SELECT c.id, s.depth + 1
		      FROM categories c
		      JOIN subtree s ON c.parent_id = s.id
		     WHERE s.depth < %d
		)
		SELECT p.id
		  FROM products p
		 WHERE p.storage_id = $2
		   AND p.category_id IN (SELECT id FROM subtree)`, maxTreeDepth),
		categoryID, storageID)
	if err != nil {
		return 0, fmt.Errorf("store: find products under category: %w", err)
	}

	var productIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan product under category: %w", err)
		}
		productIDs = append(productIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: find products under category: %w", err)
	}

	total := 0
	for _, productID := range productIDs {
		affected, err := s.RecomputeDerivedExpiry(ctx, storageID, productID)
		if err != nil {
			return total, err
		}
		total += affected
	}
	return total, nil
}

// SetBatchExpiration edits or clears one batch's expiration date.
//
// Either way the source becomes 'user': the point of the column is to record
// that a person decided, and clearing a date is as much a decision as setting
// one. A NULL date with a 'user' source is the sticky "this has no expiry"
// state that no later cascade may undo.
func (s *Store) SetBatchExpiration(ctx context.Context, storageID, batchID uuid.UUID, date *time.Time) (*Batch, error) {
	var out *Batch

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireBatchInStorage(ctx, tx, storageID, batchID); err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `
			UPDATE inventory_batches
			   SET expiration_date = $1, expiration_source = 'user'
			 WHERE id = $2
			RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at`,
			date, batchID)

		batch, err := scanBatch(row)
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

// ResetBatchExpirationToDerived undoes a manual edit and puts the batch back
// under the automatic rules.
//
// docs/specs/08-expiration-and-classification.md makes this required rather
// than optional: without a way back, one accidental edit would opt a batch out
// of every future rule change permanently, and the user would have no way to
// tell the system it was a mistake.
func (s *Store) ResetBatchExpirationToDerived(ctx context.Context, storageID, batchID uuid.UUID) (*Batch, error) {
	var out *Batch

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var productID uuid.UUID
		var createdAt time.Time
		err := tx.QueryRow(ctx, `
			SELECT b.product_id, b.created_at
			  FROM inventory_batches b
			  JOIN products p ON p.id = b.product_id
			 WHERE b.id = $1 AND p.storage_id = $2
			 FOR UPDATE OF b`, batchID, storageID).Scan(&productID, &createdAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: load batch to reset: %w", err)
		}

		rules, err := expiryRulesFor(ctx, tx, storageID, productID)
		if err != nil {
			return err
		}
		date := expiry.DateFor(createdAt, expiry.Resolve(rules))

		row := tx.QueryRow(ctx, `
			UPDATE inventory_batches
			   SET expiration_date = $1, expiration_source = 'derived'
			 WHERE id = $2
			RETURNING id, product_id, location_id, quantity, expiration_date, expiration_source, created_at`,
			date, batchID)

		batch, err := scanBatch(row)
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

// requireBatchInStorage confirms a batch belongs to this storage, through the
// product that owns it — inventory_batches has no storage_id of its own.
func requireBatchInStorage(ctx context.Context, q querier, storageID, batchID uuid.UUID) error {
	var found bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM inventory_batches b
		      JOIN products p ON p.id = b.product_id
		     WHERE b.id = $1 AND p.storage_id = $2
		)`, batchID, storageID).Scan(&found)
	if err != nil {
		return fmt.Errorf("store: check batch in storage: %w", err)
	}
	if !found {
		return ErrNotFound
	}
	return nil
}
