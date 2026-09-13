package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
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
