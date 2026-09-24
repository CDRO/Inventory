package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// InventoryBatchRow is one row of the whole-inventory read
// (docs/specs/33-inventory-overview-table.md): a batch plus every name
// inventory.html needs to render it without a second request.
type InventoryBatchRow struct {
	Batch
	ProductName  string
	ImageURL     *string
	CategoryID   *uuid.UUID
	CategoryName *string
	// LocationPath is root to leaf, e.g. ["Basement", "Right Shelf", "Layer 2"],
	// resolved by the same bounded ancestor walk ExpiringBatchesBefore uses.
	LocationPath []string
}

// InventoryBatchFilter narrows ListInventoryBatches. All fields are optional
// and combinable (docs/specs/33-inventory-overview-table.md).
type InventoryBatchFilter struct {
	// LocationID matches batches at this location or any descendant.
	LocationID *uuid.UUID
	// CategoryID matches batches whose product is in this category or any
	// descendant.
	CategoryID *uuid.UUID
	// Q is a case-insensitive substring match on the product name, using
	// idx_products_name_trgm.
	Q string
}

// ListInventoryBatches returns one storage's whole inventory, one row per
// batch, ordered by batch id (UUIDv7, so roughly by creation time) — the
// shared id-only cursor (internal/httpapi/pagination.go), never a composite
// key. Display order (urgency, name, location) is the client's job
// (docs/specs/33-inventory-overview-table.md); this order exists only so the
// cursor is stable.
//
// A location_id or category_id naming a row in another storage, or no row at
// all, is ErrNotFound — the same identical 404 requireSameStorage gives every
// other filter id in the system.
func (s *Store) ListInventoryBatches(ctx context.Context, storageID uuid.UUID, filter InventoryBatchFilter, after *uuid.UUID, limit int) ([]InventoryBatchRow, error) {
	var locationIDs []uuid.UUID
	if filter.LocationID != nil {
		if err := requireSameStorage(ctx, s.pool, treeLocations, storageID, *filter.LocationID); err != nil {
			return nil, err
		}
		ids, err := locationSubtree(ctx, s.pool, *filter.LocationID)
		if err != nil {
			return nil, err
		}
		locationIDs = ids
	}

	var categoryIDs []uuid.UUID
	if filter.CategoryID != nil {
		if err := requireSameStorage(ctx, s.pool, treeCategories, storageID, *filter.CategoryID); err != nil {
			return nil, err
		}
		ids, err := categorySubtree(ctx, s.pool, *filter.CategoryID)
		if err != nil {
			return nil, err
		}
		categoryIDs = ids
	}

	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT l.id AS origin_id, l.id, l.parent_id, l.name, 0 AS depth
			  FROM locations l
			 WHERE l.id IN (
			     SELECT DISTINCT b.location_id
			       FROM inventory_batches b
			       JOIN products p ON p.id = b.product_id
			      WHERE p.storage_id = $1
			 )
			UNION ALL
			SELECT chain.origin_id, l.id, l.parent_id, l.name, chain.depth + 1
			  FROM locations l
			  JOIN chain ON l.id = chain.parent_id
			 WHERE chain.depth < $2
		),
		paths AS (
			SELECT origin_id, array_agg(name ORDER BY depth DESC) AS path
			  FROM chain
			 GROUP BY origin_id
		)
		SELECT b.id, b.product_id, b.location_id, b.quantity, b.expiration_date,
		       b.expiration_source, b.created_at,
		       p.name, p.image_url, p.category_id, c.name,
		       coalesce(paths.path, ARRAY[]::text[])
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		  LEFT JOIN categories c ON c.id = p.category_id
		  LEFT JOIN paths ON paths.origin_id = b.location_id
		 WHERE p.storage_id = $1
		   AND ($3::uuid[] IS NULL OR b.location_id = ANY($3))
		   AND ($4::uuid[] IS NULL OR p.category_id = ANY($4))
		   AND ($5 = '' OR p.name ILIKE '%' || $5 || '%')
		   AND ($6::uuid IS NULL OR b.id > $6)
		 ORDER BY b.id
		 LIMIT $7`,
		storageID, maxTreeDepth, locationIDs, categoryIDs, filter.Q, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list inventory batches: %w", err)
	}
	defer rows.Close()

	out := []InventoryBatchRow{}
	for rows.Next() {
		var row InventoryBatchRow
		var source string
		if err := rows.Scan(&row.ID, &row.ProductID, &row.LocationID, &row.Quantity, &row.ExpirationDate,
			&source, &row.CreatedAt, &row.ProductName, &row.ImageURL, &row.CategoryID, &row.CategoryName,
			&row.LocationPath); err != nil {
			return nil, fmt.Errorf("store: scan inventory batch: %w", err)
		}
		row.ExpirationSource = ExpirationSource(source)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list inventory batches: %w", err)
	}
	return out, nil
}
