package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/gamification"
)

// ReorderProduct is one row of the reorder dashboard's source data: a product
// tracked for reorder (docs/specs/10-reorder-and-shopping-export.md), with its
// live stock alongside its threshold.
type ReorderProduct struct {
	ProductID    uuid.UUID
	Name         string
	CurrentStock int
	MinStock     int
}

// ReorderProducts returns every product in storageID with min_stock > 0 —
// "not tracked for reorder" is the whole meaning of a zero threshold, so a row
// with min_stock = 0 never reaches this result at all, rather than being
// filtered out downstream where a future caller could forget the rule.
//
// current_stock is computed in the same query via a left join and a sum, never
// read from a stored counter: the acceptance criterion in spec 10 is that
// current_stock cannot drift, and the only way to guarantee that is to never
// store it. Both the dashboard and the CSV/PDF export call this one method, so
// there is exactly one place the low-stock and out-of-stock classification can
// be computed and no second query to drift from it.
func (s *Store) ReorderProducts(ctx context.Context, storageID uuid.UUID) ([]ReorderProduct, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.name, p.min_stock, coalesce(sum(b.quantity), 0) AS current_stock
		  FROM products p
		  LEFT JOIN inventory_batches b ON b.product_id = p.id
		 WHERE p.storage_id = $1
		   AND p.min_stock > 0
		 GROUP BY p.id, p.name, p.min_stock
		 ORDER BY p.name`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: reorder products: %w", err)
	}
	defer rows.Close()

	out := []ReorderProduct{}
	for rows.Next() {
		var r ReorderProduct
		if err := rows.Scan(&r.ProductID, &r.Name, &r.MinStock, &r.CurrentStock); err != nil {
			return nil, fmt.Errorf("store: scan reorder product: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProductMinStock reads a single product's current threshold, without the
// rest of the row.
//
// The reorder "Add item" match preview needs this: a product can be a
// confident local match while sitting outside both dashboard buckets (already
// well-stocked, or not yet tracked at min_stock = 0), so its min_stock cannot
// be read back from ReorderProducts. Showing the caller a stale default
// instead — the UI's alternative — is what makes "confirm without editing"
// silently overwrite a real threshold with 1.
func (s *Store) ProductMinStock(ctx context.Context, storageID, id uuid.UUID) (int, error) {
	var minStock int
	err := s.pool.QueryRow(ctx, `
		SELECT min_stock FROM products WHERE id = $1 AND storage_id = $2`, id, storageID).Scan(&minStock)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("store: product min stock: %w", err)
	}
	return minStock, nil
}

// UpdateProductMinStock sets a product's reorder threshold.
//
// This is the write side of "adding a name that already exists adjusts its
// min_stock instead of creating a second product"
// (docs/specs/10-reorder-and-shopping-export.md): the add-item flow calls this
// on a matched product rather than CreateProduct, so no duplicate row and no
// inventory_batches or inventory_logs row is ever created for it.
func (s *Store) UpdateProductMinStock(ctx context.Context, storageID, id uuid.UUID, minStock int) (*Product, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE products SET min_stock = $1, updated_at = now()
		 WHERE id = $2 AND storage_id = $3
		RETURNING id, storage_id, name, category_id, catalog_id, item_type,
		          default_shelf_life_days, min_stock, image_url, icon_name, created_at, updated_at`,
		minStock, id, storageID)
	return scanProduct(row)
}

// UpdateProductMinStockAsUser is UpdateProductMinStock, additionally
// recording a metadata_filled contribution when the change fills in a
// threshold that was previously unset (docs/specs/51-gamification-scoring.md).
// Raising an already-tracked product's threshold, or lowering one, earns
// nothing: the reward is for closing the gap, not for tuning a number that
// was already there.
func (s *Store) UpdateProductMinStockAsUser(ctx context.Context, storageID, id uuid.UUID, minStock int, userID uuid.UUID) (*Product, error) {
	var out *Product
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var previous int
		err := tx.QueryRow(ctx, `
			SELECT min_stock FROM products WHERE id = $1 AND storage_id = $2 FOR UPDATE`,
			id, storageID).Scan(&previous)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock product min stock: %w", err)
		}

		row := tx.QueryRow(ctx, `
			UPDATE products SET min_stock = $1, updated_at = now()
			 WHERE id = $2 AND storage_id = $3
			RETURNING id, storage_id, name, category_id, catalog_id, item_type,
			          default_shelf_life_days, min_stock, image_url, icon_name, created_at, updated_at`,
			minStock, id, storageID)
		product, err := scanProduct(row)
		if err != nil {
			return err
		}
		out = product

		if previous == 0 && minStock > 0 {
			return recordContribution(ctx, tx, storageID, userID, gamification.KindMetadataFilled, &id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
