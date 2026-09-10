package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/matching"
)

// The trigram queries backing internal/matching
// (docs/specs/07-shopping-list-reconciliation.md).
//
// # Why these filter on similarity() rather than the % operator
//
// pg_trgm's `%` operator is what uses a gin_trgm_ops index, but it compares
// against pg_trgm.similarity_threshold — a session GUC defaulting to 0.3.
// Matching's own thresholds are named constants precisely so they can be tuned
// in one visible place; routing the real cutoff through a session variable
// would mean a connection that never ran set_limit() silently matches by a
// different rule than the one the constants document, and a pooled connection
// makes that intermittent.
//
// So these use an explicit similarity(...) >= $n filter: correct regardless of
// session state, at the cost of a scan. That trade is right for this system's
// shape — products are per storage and a household has hundreds, not millions,
// and the scan is already narrowed by idx_products_storage_id. If the shared
// catalog ever grows past the point where this matters, the fix is a GiST
// index and the `<->` distance operator, which orders without a GUC at all.

// SimilarProducts returns this storage's products ranked by trigram similarity
// to text, considering only rows at or above minSimilarity.
//
// Ordering breaks ties on id so that two products with identical names produce
// a stable candidate list — otherwise the "several close candidates" check in
// internal/matching could flip between reads and show a user a different
// question each time they reload.
func (s *Store) SimilarProducts(ctx context.Context, storageID uuid.UUID, text string, minSimilarity float64, limit int) ([]matching.LocalCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, similarity(name, $2) AS sim
		  FROM products
		 WHERE storage_id = $1
		   AND similarity(name, $2) >= $3
		 ORDER BY sim DESC, id
		 LIMIT $4`, storageID, text, minSimilarity, limit)
	if err != nil {
		return nil, fmt.Errorf("store: similar products: %w", err)
	}
	defer rows.Close()

	var out []matching.LocalCandidate
	for rows.Next() {
		var c matching.LocalCandidate
		if err := rows.Scan(&c.ProductID, &c.Name, &c.Similarity); err != nil {
			return nil, fmt.Errorf("store: scan similar product: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: similar products: %w", err)
	}
	return out, nil
}

// SimilarCatalogProduct returns the best anonymous-catalog row for text, or
// nil when nothing reaches minSimilarity.
//
// The query selects display fields and the id. The id is for the server alone —
// accepting a hit copies its fields into a storage-local product, and declining
// it links a newly created catalog row to it as a variant — and must not reach
// a client; see internal/matching's package doc.
//
// Nothing here is scoped to a storage, because catalog rows have no storage.
// That is the whole design: the table describes what a product *is*, so a
// lookup cannot reveal who has one (docs/specs/02-data-model.md).
func (s *Store) SimilarCatalogProduct(ctx context.Context, text string, minSimilarity float64) (*matching.CatalogMatch, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, display_name, category_path, item_type, image_url, icon_name, default_shelf_life_days
		  FROM catalog_products
		 WHERE similarity(normalized_name, $1) >= $2
		 ORDER BY similarity(normalized_name, $1) DESC, id
		 LIMIT 1`, text, minSimilarity)

	var m matching.CatalogMatch
	err := row.Scan(&m.ID, &m.DisplayName, &m.CategoryPath, &m.ItemType,
		&m.ImageURL, &m.IconName, &m.DefaultShelfLifeDays)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not an error: a miss is the ordinary case for a product nobody has
		// described yet, and it is what tells the caller an external lookup is
		// finally warranted.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: similar catalog product: %w", err)
	}
	return &m, nil
}

// CatalogVariantsOf returns the alternatives around a catalog hit: the hit's
// siblings when it is itself a variant, or its children when it is a base
// (docs/specs/07-shopping-list-reconciliation.md).
//
// Ranked by similarity to the original text, so an abbreviated line like
// "thomatoes, c." surfaces *cherry tomatoes* ahead of *yellow tomatoes* even
// though both are equally valid siblings.
func (s *Store) CatalogVariantsOf(ctx context.Context, catalogID uuid.UUID, text string, limit int) ([]matching.CatalogVariant, error) {
	rows, err := s.pool.Query(ctx, `
		WITH hit AS (
		    SELECT id, base_id FROM catalog_products WHERE id = $1
		)
		SELECT c.id, c.display_name
		  FROM catalog_products c, hit
		 WHERE c.id <> hit.id
		   AND ( (hit.base_id IS NOT NULL AND c.base_id = hit.base_id)
		      OR (hit.base_id IS NULL     AND c.base_id = hit.id) )
		 ORDER BY similarity(c.normalized_name, $2) DESC, c.display_name
		 LIMIT $3`, catalogID, text, limit)
	if err != nil {
		return nil, fmt.Errorf("store: catalog variants: %w", err)
	}
	defer rows.Close()

	var out []matching.CatalogVariant
	for rows.Next() {
		var v matching.CatalogVariant
		if err := rows.Scan(&v.ID, &v.DisplayName); err != nil {
			return nil, fmt.Errorf("store: scan catalog variant: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: catalog variants: %w", err)
	}
	return out, nil
}
